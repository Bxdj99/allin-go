package gateway

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"allin/internal/config"
)

// 账号健康检查。
//
// 目的：用户看到一堆账号「冷却中」时，分不清是临时限流还是已经废了。
// 这里主动发一次最小请求，给出明确结论：
//   - ok           能用
//   - no-credit    能登录但额度用尽
//   - relogin      凭据已被上游判死，必须重新登录
//   - skip         没有凭据，跳过

// healthProbeTimeout 是单个账号的探测超时。
const healthProbeTimeout = 25 * time.Second

// handleAccountsProbe 检测所有（或指定供应商的）账号是否还能用。
//
//	POST /api/accounts/probe        {"providerId":"可选"}
//
// 会打真实上游（模型目录接口，不消耗额度），所以是显式触发，不做后台轮询。
func (s *Server) handleAccountsProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var in struct {
		ProviderID string `json:"providerId"`
	}
	_ = decodeJSON(r, &in)

	cfg := s.store.Snapshot()

	type job struct {
		pid  string
		prov *config.Provider
		acc  *config.Account
	}
	var jobs []job
	for _, p := range cfg.Providers {
		if p == nil || !p.IsEnabled() {
			continue
		}
		if in.ProviderID != "" && p.ID != in.ProviderID {
			continue
		}
		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			jobs = append(jobs, job{pid: p.ID, prov: p, acc: a})
		}
	}

	type row struct {
		ProviderID string `json:"providerId"`
		AccountID  string `json:"accountId"`
		Name       string `json:"name,omitempty"`
		Status     string `json:"status"`
		Detail     string `json:"detail,omitempty"`
	}

	rows := make([]row, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // 并发上限，避免一次打爆上游

	for i, j := range jobs {
		wg.Add(1)
		go func(idx int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			rows[idx] = row{ProviderID: j.pid, AccountID: j.acc.ID, Name: j.acc.Name}

			if !j.acc.IsEnabled() {
				rows[idx].Status = "disabled"
				rows[idx].Detail = "账号已禁用"
				return
			}
			if j.acc.Key() == "" {
				rows[idx].Status = "skip"
				rows[idx].Detail = "没有凭据"
				return
			}

			ctx, cancel := context.WithTimeout(r.Context(), healthProbeTimeout)
			defer cancel()

			switch {
			case isWorkBuddyProviderID(j.pid):
				// 用聊天接口探测，而不是积分接口。
				//
				// 积分接口（billing）和聊天接口（chat）是两条独立链路，
				// 可能出现 billing 能查、chat 却 401 的情况（实测遇到过）。
				// 体检的目的就是回答「这个号还能不能用来聊天」，
				// 所以必须打聊天链路，否则会给出乐观的错误结论。
				cust := wbCustomerFromAccount(j.prov, j.acc)
				probeBody := map[string]any{
					"model":      probeModel(j.prov),
					"messages":   []map[string]any{{"role": "user", "content": "hi"}},
					"max_tokens": 1,
					"stream":     true,
				}
				raw, perr := cust.Chat(ctx, probeBody)
				switch {
				case perr != nil && isAuthErrText(perr.Error()):
					rows[idx].Status = "relogin"
					rows[idx].Detail = "聊天接口返回 401，凭据已失效，需要重新登录"
					s.pool.MarkNeedRelogin(j.pid, j.acc.ID, true)
				case perr != nil:
					rows[idx].Status = "error"
					rows[idx].Detail = perr.Error()
				case probeSaysNoCredit(raw):
					rows[idx].Status = "no-credit"
					rows[idx].Detail = "额度已用尽"
					s.pool.MarkNeedRelogin(j.pid, j.acc.ID, false)
				default:
					rows[idx].Status = "ok"
					rows[idx].Detail = "聊天链路可用"
					s.pool.MarkNeedRelogin(j.pid, j.acc.ID, false)
				}
			default:
				// 其它供应商不做网络探测，但也不能一律报「健康」。
				//
				// 这正是用户反馈里那句「trae 后台账号显示健康，对话出不来」的来源：
				// 以前这里无条件 ok，token 早就过期了也显示绿的。
				// 现在至少用本地 expires 判一次过期 —— 不花一个请求，
				// 但能立刻告诉用户「该续期/该重新登录了」。
				if strings.TrimSpace(j.acc.Key()) == "" {
					rows[idx].Status = "relogin"
					rows[idx].Detail = "没有凭据，需要重新登录"
					break
				}
				if exp := j.acc.Expires; exp > 0 && time.Now().UnixMilli() >= exp {
					// 过期了：能续期就提示续期，不能续期才要求重登
					if strings.TrimSpace(j.acc.Refresh) != "" {
						rows[idx].Status = "error"
						rows[idx].Detail = "凭据已过期（有 refresh token，可点「Token 保活」续期）"
					} else {
						rows[idx].Status = "relogin"
						rows[idx].Detail = "凭据已过期且没有 refresh token，需要重新登录"
					}
					break
				}
				rows[idx].Status = "ok"
				rows[idx].Detail = "有凭据（未做网络探测）"
			}
		}(i, j)
	}
	wg.Wait()

	var okCount, reloginCount, noCreditCount int
	for _, rw := range rows {
		switch rw.Status {
		case "ok":
			okCount++
		case "relogin":
			reloginCount++
		case "no-credit":
			noCreditCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"rows":     rows,
		"total":    len(rows),
		"usable":   okCount,
		"relogin":  reloginCount,
		"noCredit": noCreditCount,
		"note":     "relogin 表示凭据已被上游判失效，需要重新登录；no-credit 表示额度用尽",
	})
}

// probeModel 挑一个用来探测的模型。
//
// 优先用供应商配置里的第一个模型；没有就退回一个通用名，
// 探测只关心「有没有被挡」，模型不存在也是一种有效信号（不是 401）。
func probeModel(p *config.Provider) string {
	if p != nil {
		for _, m := range p.Models {
			if m != "" {
				return m
			}
		}
	}
	return "deepseek-v4.1-flash"
}

// probeSaysNoCredit 判断探测响应是否表示「额度用尽」。
func probeSaysNoCredit(raw []byte) bool {
	l := strings.ToLower(string(raw))
	for _, kw := range []string{"insufficient_quota", "quota_exceeded", "额度", "no credit", `"code":6004`} {
		if strings.Contains(l, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// isWorkBuddyProviderID 判断是不是 WorkBuddy 供应商。
func isWorkBuddyProviderID(pid string) bool {
	return pid == "workbuddy" || pid == "workbuddy-ai"
}

// isAuthErrText 判断错误文本是否属于鉴权失败。
func isAuthErrText(msg string) bool {
	for _, kw := range []string{"401", "403", "Unauthorized", "Authorization Required"} {
		if containsFold(msg, kw) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	ls, lsub := []byte(s), []byte(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		ok := true
		for j := 0; j < len(lsub); j++ {
			a, b := ls[i+j], lsub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// trimFloat 把浮点余额压成好看的形式（最多两位小数，去掉尾随 0）。
func trimFloat(f float64) string {
	r := round2f(f)
	s := strconv.FormatFloat(r, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}
