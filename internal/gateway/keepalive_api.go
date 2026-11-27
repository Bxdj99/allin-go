package gateway

import (
	"context"
	"net/http"
	"time"
)

// 手动触发一次 Token 保活。
//
//	POST /api/tokens/keepalive            对所有供应商跑一轮
//	POST /api/tokens/keepalive?provider=x 只跑指定供应商
//
// 为什么需要它：自动续期只在「快到期」时才动手（tokenRefreshLead）。
// 但有时用户就是想立刻把一批 token 换新 —— 比如刚从别处拿到更新鲜的版本、
// 或者怀疑某个账号不新鲜。wb2api 那边就有一个「立即保活」按钮，
// 这里对齐同样的能力。
//
// 与自动续期的区别：这里不看剩余有效期，所有带 refresh 的账号都续一次。
func (s *Server) handleTokenKeepalive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}

	wantPID := r.URL.Query().Get("provider")

	// 串行化：和自动续期共用一把锁，避免两边同时对同一账号发起刷新
	if !refreshMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "自动续期正在进行，稍后再试",
		})
		return
	}
	defer refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	cfg := s.store.Snapshot()
	if cfg == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "读不到配置"})
		return
	}

	type result struct {
		Provider  string `json:"provider"`
		AccountID string `json:"accountId"`
		Name      string `json:"name,omitempty"`
		OK        bool   `json:"ok"`
		Skipped   string `json:"skipped,omitempty"`
		Error     string `json:"error,omitempty"`
	}

	var results []result
	okCount, skipCount, failCount := 0, 0, 0
	changed := false

	for _, p := range cfg.Providers {
		if p == nil || !p.IsEnabled() {
			continue
		}
		if wantPID != "" && p.ID != wantPID {
			continue
		}
		for _, a := range p.Accounts {
			if a == nil || !a.IsEnabled() {
				continue
			}
			row := result{Provider: p.ID, AccountID: a.ID, Name: a.Name}

			// 没有 refresh 就没法续期 —— 这不是错误，是事实
			if a.Refresh == "" {
				row.Skipped = "没有 refresh token（只能重新登录）"
				results = append(results, row)
				skipCount++
				continue
			}

			err := s.refreshOne(ctx, p, a)
			if err == nil {
				row.OK = true
				changed = true
				okCount++
				// 续期成功顺手清掉「需重登」标记，否则账号永远被跳过
				if s.pool.NeedRelogin(p.ID, a.ID) {
					s.pool.MarkNeedRelogin(p.ID, a.ID, false)
				}
			} else if _, isSkip := err.(skipRefreshErr); isSkip {
				row.Skipped = "该供应商不支持自动刷新"
				skipCount++
			} else {
				row.Error = err.Error()
				failCount++
			}
			results = append(results, row)
			// 节流：避免同一时刻连打上游
			time.Sleep(600 * time.Millisecond)
		}
	}

	if changed {
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "error": "续期成功但保存失败: " + err.Error(), "results": results,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      failCount == 0,
		"success": okCount,
		"skipped": skipCount,
		"failed":  failCount,
		"total":   len(results),
		"results": results,
		"hint":    "失败的通常是 refresh token 被上游吊销（12153 revoked），只能重新登录",
	})
}
