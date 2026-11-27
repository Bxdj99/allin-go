package gateway

import (
	"context"
	"net/http"
	"time"

	"allin/internal/config"
)

// Token 保活：把所有带 refresh 的账号立刻续期一遍。
//
// 与自动续期的分工（v1.55.0）：
//   - 定时全量保活：每天固定时刻（默认 22:00）把所有账号续一遍，
//     token 始终新鲜，不会出现「刚好在请求时过期」。
//   - 10 分钟巡检：只刷「快到期」的，作为兜底。
//
// 两者共用 refreshMu，不会同时对同一账号发起刷新。

// keepaliveResult 是单个账号的保活结果。
type keepaliveResult struct {
	Provider  string `json:"provider"`
	AccountID string `json:"accountId"`
	Name      string `json:"name,omitempty"`
	OK        bool   `json:"ok"`
	Skipped   string `json:"skipped,omitempty"`
	Error     string `json:"error,omitempty"`
}

// keepaliveAll 对所有（或指定供应商的）带 refresh 账号做一次续期。
//
// 调用方需自行持有 refreshMu，避免与自动续期打架。
func (s *Server) keepaliveAll(ctx context.Context, wantPID string) (cfg *config.Config, results []keepaliveResult, okN, skipN, failN int, changed bool) {
	cfg = s.store.Snapshot()
	if cfg == nil {
		return nil, nil, 0, 0, 0, false
	}

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
			row := keepaliveResult{Provider: p.ID, AccountID: a.ID, Name: a.Name}

			// 没有 refresh 就没法续期 —— 这不是错误，是事实
			if a.Refresh == "" {
				row.Skipped = "没有 refresh token（只能重新登录）"
				results = append(results, row)
				skipN++
				continue
			}

			err := s.refreshOne(ctx, p, a)
			switch {
			case err == nil:
				row.OK = true
				changed = true
				okN++
				// 续期成功顺手清掉「需重登」标记，否则账号永远被跳过
				if s.pool.NeedRelogin(p.ID, a.ID) {
					s.pool.MarkNeedRelogin(p.ID, a.ID, false)
				}
			case isSkipRefresh(err):
				row.Skipped = "该供应商不支持自动刷新"
				skipN++
			default:
				row.Error = err.Error()
				failN++
			}
			results = append(results, row)
			// 节流：避免同一时刻连打上游
			time.Sleep(600 * time.Millisecond)
		}
	}
	return cfg, results, okN, skipN, failN, changed
}

// isSkipRefresh 判断错误是否属于「该供应商不支持续期」。
func isSkipRefresh(err error) bool {
	_, ok := err.(skipRefreshErr)
	return ok
}

// handleTokenKeepalive 手动触发一次保活。
//
//	POST /api/tokens/keepalive            对所有供应商跑一轮
//	POST /api/tokens/keepalive?provider=x 只跑指定供应商
func (s *Server) handleTokenKeepalive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}

	if !refreshMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "自动续期正在进行，稍后再试",
		})
		return
	}
	defer refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	cfg, results, okN, skipN, failN, changed := s.keepaliveAll(ctx, r.URL.Query().Get("provider"))

	if changed && cfg != nil {
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "error": "续期成功但保存失败: " + err.Error(), "results": results,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      failN == 0,
		"success": okN,
		"skipped": skipN,
		"failed":  failN,
		"total":   len(results),
		"results": results,
		"hint":    "失败的通常是 refresh token 被上游吊销（12153 revoked），只能重新登录",
	})
}
