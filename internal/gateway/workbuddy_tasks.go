// workbuddy_tasks.go WorkBuddy 任务中心的 HTTP 入口。
//
//	POST /api/workbuddy/tasks         {"accountId":"可选","provider":"可选"}  → 跑全套
//	GET  /api/workbuddy/tasks/status?accountId=可选                          → 只读汇总
//
// 写法照着 handleWorkBuddyCheckin：多供应商/多账号循环、逐行结果表、写回配置。
// 签到本身不在这里重写——它由既有的 doCheckin 提供，作为 RunAllTasks 的第 2 步
// 通过闭包注入。
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"allin/internal/config"
	"allin/internal/provider/workbuddy"
)

// taskAccountGap 账号之间的等待。全套跑十来个请求，账号一多容易踩风控。
const taskAccountGap = 800 * time.Millisecond

// taskNames 是 RunAllTasks 会返回的任务名，用来把"这次没跑的任务"标记清楚。
var taskNames = []string{
	"report", "checkin", "streak", "claim-gift", "compensation", "makeup",
	"buddy", "travel-depart", "travel-claim", "lottery",
}

// wbCustomerFromAccount 把配置里的账号转成 provider 层认识的 Customer。
//
// global 判定：供应商 ID 是 workbuddy-ai，或账号 realm 明确写了 global。
// 两者都不满足就当 CN 处理——CN 是绝大多数情况。
func wbCustomerFromAccount(p *config.Provider, a *config.Account) workbuddy.Customer {
	global := p != nil && p.ID == "workbuddy-ai"
	if a != nil && strings.EqualFold(strings.TrimSpace(a.Realm), "global") {
		global = true
	}
	if a == nil {
		return workbuddy.NewCustomer("", "", "", "", "", global)
	}
	// 企业 ID 与设备令牌在 config.Account 里没有对应字段（登录时拿到了但没落盘），
	// 这里留空：两者都只在"有则更好"的位置，缺了不影响 CN 个人号的任务链路。
	return workbuddy.NewCustomer(a.UID, a.Key(), a.Domain, "", "", global)
}

// wbOriginOf 返回供应商对应的上游根地址（签到用，沿用既有口径）。
func wbOriginOf(p *config.Provider) string {
	origin := "https://copilot.tencent.com"
	if p != nil && p.ID == "workbuddy-ai" {
		origin = "https://www.workbuddy.ai"
	}
	if p != nil && p.BaseURL != "" {
		// baseUrl 一般带 /v1，签到路径在根上
		origin = strings.TrimSuffix(strings.TrimSuffix(p.BaseURL, "/v1"), "/")
	}
	return origin
}

// handleWorkBuddyTasks 跑全套任务。
//
//	POST /api/workbuddy/tasks {"accountId":"wb-xxx","provider":"workbuddy","skipReport":false}
//
// body 全可空：空 body = 跑所有 workbuddy 供应商下的所有启用账号。
func (s *Server) handleWorkBuddyTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST；只读查询用 /api/workbuddy/tasks/status"})
		return
	}

	var opt struct {
		Provider   string `json:"provider"`
		AccountID  string `json:"accountId"`
		SkipReport bool   `json:"skipReport"`
		NoCheckin  bool   `json:"noCheckin"`
		// TravelWaitSec 派出后等多久再查一次旅行状态（秒）。
		TravelWaitSec int `json:"travelWaitSec"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&opt)
	}
	pid := strings.TrimSpace(opt.Provider)
	if pid == "" {
		pid = strings.TrimSpace(r.URL.Query().Get("provider"))
	}

	cfg := s.store.Snapshot()
	targets := wbTargetProviders(cfg, pid)
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "total": 0, "results": []any{},
			"note": "配置里没有 workbuddy / workbuddy-ai 供应商",
		})
		return
	}

	var results []map[string]any
	var okCount, failedCount, skippedCount int
	changed := false

	for _, p := range targets {
		origin := wbOriginOf(p)
		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			row := map[string]any{"provider": p.ID, "accountId": a.ID, "name": a.Name}
			if !a.IsEnabled() {
				row["ok"] = false
				row["skipped"] = "disabled"
				results = append(results, row)
				skippedCount++
				continue
			}
			if a.UID == "" || a.Key() == "" {
				row["ok"] = false
				row["skipped"] = "no_credential"
				results = append(results, row)
				skippedCount++
				continue
			}
			if opt.AccountID != "" && a.ID != opt.AccountID {
				continue
			}

			cust := wbCustomerFromAccount(p, a)
			accID := a.ID
			opts := workbuddy.TaskOptions{
				SkipReport:  opt.SkipReport,
				SkipCheckin: opt.NoCheckin,
				TravelWait:  time.Duration(opt.TravelWaitSec) * time.Second,
				QuestGap:    taskAccountGap,
			}
			if !opt.NoCheckin {
				// 注入既有签到实现：闭包捕获 origin 与账号指针，返回 Outcome。
				opts.Checkin = func(ctx context.Context) workbuddy.Outcome {
					cur := findAccount(s.store.Snapshot(), p.ID, accID)
					if cur == nil {
						return workbuddy.Outcome{Status: workbuddy.OutcomeFailed, Message: "账号已被移除"}
					}
					credit, streak, outcome, err := s.doCheckin(origin, cur)
					if err != nil {
						return workbuddy.Outcome{Status: workbuddy.OutcomeFailed, Message: err.Error()}
					}
					status := workbuddy.OutcomeOK
					msg := "签到成功"
					if outcome == "already" {
						status = workbuddy.OutcomeAlready
						msg = "今天已签到"
					}
					return workbuddy.Outcome{
						Status:  status,
						Message: msg + "（连登 " + itoaSmall(streak) + " 天）",
						Credit:  int64(credit),
					}
				}
			}

			// 每个账号最多跑 3 分钟（十来次请求 + 节流；抽奖次数多时会长一点）。
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
			items := workbuddy.RunAllTasks(ctx, cust, opts)
			cancel()

			// 写回配置：tasksRun / taskOutcomeDetail / lastTaskRun。
			cur := findAccount(cfg, p.ID, accID)
			if cur != nil {
				if cur.TasksRun == nil {
					cur.TasksRun = map[string]string{}
				}
				if cur.TaskOutcomeDetail == nil {
					cur.TaskOutcomeDetail = map[string]string{}
				}
				totalCredit := int64(0)
				var failed []string
				for _, it := range items {
					cur.TasksRun[it.Name] = it.Outcome.Status
					if it.Outcome.Message != "" {
						cur.TaskOutcomeDetail[it.Name] = it.Outcome.Message
					}
					totalCredit += it.Outcome.Credit
					if it.Outcome.Status == workbuddy.OutcomeFailed {
						failed = append(failed, it.Name)
					}
				}
				cur.LastTaskRun = time.Now().Unix()
				row["credit"] = totalCredit
				if len(failed) > 0 {
					row["failedTasks"] = failed
				}
				changed = true
			}

			row["ok"] = true
			row["tasks"] = items
			if len(items) == 0 {
				row["note"] = "没有任务被执行"
			}
			// 逐项看有没有 failed：有就把这一行标成失败（"ok" 表示整行干净）。
			hasFail := false
			for _, it := range items {
				if it.Outcome.Status == workbuddy.OutcomeFailed {
					hasFail = true
					break
				}
			}
			if hasFail {
				row["ok"] = false
				failedCount++
			} else {
				okCount++
			}
			results = append(results, row)
			time.Sleep(taskAccountGap)
		}
	}

	if changed {
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "error": "任务已执行但配置落盘失败: " + err.Error(), "results": results,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        failedCount == 0,
		"total":     len(results),
		"succeeded": okCount,
		"failed":    failedCount,
		"skipped":   skippedCount,
		"results":   results,
	})
}

// handleWorkBuddyTaskStatus 只读汇总：info / streak / lottery / compensation / travel。
//
//	GET /api/workbuddy/tasks/status?provider=workbuddy&accountId=wb-xxx
//
// 不写配置、不改账号，纯粹把上游当前状态拉回来。
func (s *Server) handleWorkBuddyTaskStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET"})
		return
	}
	pid := strings.TrimSpace(r.URL.Query().Get("provider"))
	aid := strings.TrimSpace(r.URL.Query().Get("accountId"))

	cfg := s.store.Snapshot()
	targets := wbTargetProviders(cfg, pid)
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "total": 0, "results": []any{},
			"note": "配置里没有 workbuddy / workbuddy-ai 供应商",
		})
		return
	}

	var results []map[string]any
	for _, p := range targets {
		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			if aid != "" && a.ID != aid {
				continue
			}
			row := map[string]any{"provider": p.ID, "accountId": a.ID, "name": a.Name}
			if !a.IsEnabled() {
				row["ok"] = false
				row["skipped"] = "disabled"
				results = append(results, row)
				continue
			}
			if a.UID == "" || a.Key() == "" {
				row["ok"] = false
				row["skipped"] = "no_credential"
				results = append(results, row)
				continue
			}
			cust := wbCustomerFromAccount(p, a)
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			snap := workbuddy.GetStatus(ctx, cust, 300*time.Millisecond)
			cancel()

			row["ok"] = true
			row["status"] = snap
			// 本地记录：上次跑全套的时间与逐项结论（可能为空）。
			row["lastTaskRun"] = a.LastTaskRun
			row["tasksRun"] = a.TasksRun
			row["taskOutcomeDetail"] = a.TaskOutcomeDetail
			// 签到记录沿用既有字段，方便面板一起看。
			row["checkinDate"] = a.CheckinDate
			row["checkinOutcome"] = a.CheckinOutcome
			row["checkinStreak"] = a.CheckinStreak
			results = append(results, row)
			time.Sleep(300 * time.Millisecond)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"total":   len(results),
		"results": results,
	})
}

// wbTargetProviders 按可选的 provider ID 选出要处理的供应商列表。
// 空 ID = 所有 workbuddy / workbuddy-ai 供应商。
func wbTargetProviders(cfg *config.Config, pid string) []*config.Provider {
	if pid != "" {
		if p := cfg.FindProvider(pid); p != nil {
			return []*config.Provider{p}
		}
		return nil
	}
	var out []*config.Provider
	for _, p := range cfg.Providers {
		if p != nil && (p.ID == "workbuddy" || p.ID == "workbuddy-ai") {
			out = append(out, p)
		}
	}
	return out
}

// findAccount 在配置快照里按 providerID + accountID 找账号（O(账号数)）。
func findAccount(cfg *config.Config, pid, aid string) *config.Account {
	p := cfg.FindProvider(pid)
	if p == nil {
		return nil
	}
	for _, a := range p.Accounts {
		if a != nil && a.ID == aid {
			return a
		}
	}
	return nil
}

// itoaSmall 是个极小的整数转字符串（避免为了它引 strconv 到热路径外的这里）。
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
