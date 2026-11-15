package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"allin/internal/provider/workbuddy"
)

// 模型目录诊断。
//
//	GET /api/workbuddy/catalog/diagnose
//
// 对每个账号 × 每个站点 × 每个端点各探一次，把「拿到了几个」摊开给用户看。
// 这样「为什么我只看到 3 个模型」这类问题能自己定位：
//   - 某站点/端点返回 0 → 看 Error 字段
//   - 各站点数量差很多 → 说明账号所在区域决定了默认目录
func (s *Server) handleCatalogDiagnose(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	only := strings.TrimSpace(r.URL.Query().Get("accountId"))

	type accTarget struct {
		label     string
		providerID string
		acc       workbuddy.Account
	}
	var targets []accTarget
	for _, p := range cfg.Providers {
		if p == nil || (p.ID != "workbuddy" && p.ID != "workbuddy-ai") {
			continue
		}
		origin := p.BaseURL
		if strings.TrimSpace(origin) == "" {
			origin = "https://copilot.tencent.com"
		}
		for _, a := range p.Accounts {
			if a == nil || !a.IsEnabled() || a.Key() == "" || a.UID == "" {
				continue
			}
			if only != "" && a.ID != only {
				continue
			}
			label := a.Name
			if label == "" {
				label = a.ID
			}
			targets = append(targets, accTarget{
				label: label, providerID: p.ID,
				acc: workbuddy.Account{
					UID: a.UID, Token: a.Key(),
					Domain: a.Domain, Realm: a.Realm,
				},
			})
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "results": []any{},
			"note": "没有可诊断的 WorkBuddy 账号",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	type row struct {
		idx  int
		data map[string]any
	}
	ch := make(chan row, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(idx int, tg accTarget) {
			defer wg.Done()
			acc := tg.acc
			cr, err := workbuddy.FetchCatalogWithProbes(ctx, "", &acc)
			out := map[string]any{
				"accountId": tg.label,
				"provider":  tg.providerID,
			}
			if err != nil {
				out["ok"] = false
				out["error"] = err.Error()
			} else {
				out["ok"] = true
				out["total"] = len(cr.Models)
				out["probes"] = cr.Probes
				// 顺手把模型名也给出来，方便和面板对比
				names := make([]string, 0, len(cr.Models))
				for _, m := range cr.Models {
					names = append(names, m.ID)
				}
				out["models"] = names
			}
			ch <- row{idx: idx, data: out}
		}(i, t)
	}
	go func() { wg.Wait(); close(ch) }()

	out := make([]map[string]any, len(targets))
	for r := range ch {
		out[r.idx] = r.data
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"results":   out,
		"hint":      "每个账号会同时探国内站与国际站；数量差异说明账号区域影响默认目录，实际取并集",
		"checkedAt": time.Now().Unix(),
	})
}
