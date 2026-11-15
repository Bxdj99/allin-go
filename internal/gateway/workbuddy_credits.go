package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"allin/internal/provider/workbuddy"
)

// WorkBuddy 积分查询接口。
//
//	GET /api/workbuddy/credits            查全部账号（并发，最多等 20 秒）
//	GET /api/workbuddy/credits?accountId= 只查一个
func (s *Server) handleWorkBuddyCredits(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	only := strings.TrimSpace(r.URL.Query().Get("accountId"))

	// 收集要查的账号
	type target struct {
		providerID string
		name       string
		acc        *workbuddy.Customer
		accountID  string
	}
	var targets []target
	for _, p := range cfg.Providers {
		if p == nil || (p.ID != "workbuddy" && p.ID != "workbuddy-ai") {
			continue
		}
		for _, a := range p.Accounts {
			if a == nil || !a.IsEnabled() || a.UID == "" || a.Key() == "" {
				continue
			}
			if only != "" && a.ID != only {
				continue
			}
			c := workbuddy.NewCustomer(a.UID, a.Key(), a.Domain, "", "", p.ID == "workbuddy-ai")
			targets = append(targets, target{
				providerID: p.ID, name: a.Name, acc: &c, accountID: a.ID,
			})
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "results": []any{},
			"note": "没有可查询的 WorkBuddy 账号（需要已登录且启用）",
		})
		return
	}

	// 并发查询，单个失败不影响其它
	type result struct {
		idx int
		sum *workbuddy.CreditSummary
		err error
	}
	ch := make(chan result, len(targets))
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	for i, t := range targets {
		wg.Add(1)
		go func(idx int, cust workbuddy.Customer) {
			defer wg.Done()
			sum, err := workbuddy.GetCredits(ctx, cust)
			ch <- result{idx: idx, sum: sum, err: err}
		}(i, *t.acc)
	}
	go func() { wg.Wait(); close(ch) }()

	out := make([]map[string]any, len(targets))
	for res := range ch {
		t := targets[res.idx]
		row := map[string]any{
			"accountId": t.accountID,
			"name":      t.name,
			"provider":  t.providerID,
		}
		if res.err != nil {
			row["ok"] = false
			row["error"] = res.err.Error()
		} else {
			row["ok"] = true
			if res.sum != nil {
				row["total"] = res.sum.Total
				row["used"] = res.sum.Used
				row["remaining"] = res.sum.Remaining
				row["percent"] = res.sum.Percent
				row["buckets"] = res.sum.Buckets
				row["packages"] = res.sum.Packages
				row["activePackages"] = res.sum.Active
				if res.sum.ExpireAt != "" {
					row["expireAt"] = res.sum.ExpireAt
				}
				if res.sum.Note != "" {
					row["note"] = res.sum.Note
				}
			}
		}
		out[res.idx] = row
	}

	// 汇总
	var totalRemain, totalCap float64
	okCount := 0
	for _, row := range out {
		if row["ok"] == true {
			okCount++
			if v, ok := row["remaining"].(float64); ok {
				totalRemain += v
			}
			if v, ok := row["total"].(float64); ok {
				totalCap += v
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"results":      out,
		"accountCount": len(targets),
		"okCount":      okCount,
		"totalRemain":  round2f(totalRemain),
		"totalCap":     round2f(totalCap),
		"observedAt":   time.Now().Unix(),
	})
}

func round2f(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
