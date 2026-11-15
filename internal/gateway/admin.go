package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"allin/internal/config"
	"allin/internal/provider"
)

// handleStatus 返回网关运行状态。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	providers := 0
	accounts := 0
	for _, p := range cfg.Providers {
		if p != nil && p.IsEnabled() {
			providers++
			accounts += len(p.Accounts)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"uptimeSec":  int64(time.Since(s.started).Seconds()),
		"requests":   s.requests.Load(),
		"errors":     s.errors.Load(),
		"switches":   s.switches.Load(),
		"providers":  providers,
		"accounts":   accounts,
		"models":     len(provider.ModelList(cfg)),
		"configPath": s.store.Path(),
		// listen 是配置文件里的监听地址；面板拿它拼客户端连接信息（不写死 3425）
		"listen": net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port)),
		"host":   cfg.Server.Host,
		"port":   cfg.Server.Port,
	})
}

// handleConfig 读/写配置。GET 返回脱敏视图，POST 落盘。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.store.Snapshot().Redacted())
	case http.MethodPost, http.MethodPut:
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			s.fail(w, http.StatusBadRequest, "读取失败: "+err.Error())
			return
		}
		cur := s.store.Snapshot()
		var incoming config.Config
		if err := json.Unmarshal(raw, &incoming); err != nil {
			s.fail(w, http.StatusBadRequest, "配置不是合法 JSON: "+err.Error())
			return
		}
		// 口令留空表示不修改（脱敏视图里是掩码，原样回传不该把口令写坏）
		if incoming.Server.AuthKey == "" || strings.Contains(incoming.Server.AuthKey, "...") {
			incoming.Server.AuthKey = cur.Server.AuthKey
			incoming.Server.AuthKeyOrigin = cur.Server.AuthKeyOrigin
		}
		if incoming.Server.Port == 0 {
			incoming.Server.Port = cur.Server.Port
		}
		if incoming.Server.Host == "" {
			incoming.Server.Host = cur.Server.Host
		}
		// 供应商/账号的密钥同样要防"掩码写回"：面板拿到的是脱敏视图，
		// 用户只改端口时会把整份视图原样回传，这里把掩码值还原成真实值。
		mergeMaskedSecrets(cur, &incoming)
		if err := s.store.Save(&incoming); err != nil {
			s.fail(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restartNeeded": incoming.Server.Port != cur.Server.Port})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
	}
}

// handleWorkBuddyCheckin 对 WorkBuddy 供应商下的所有账号做一次每日签到。
//
// 上游协议（实测）：
//
//	POST {origin}/v2/billing/meter/daily-checkin   body {}
//	→ {"code":0,"data":{"credit":100,"streak_days":35,"is_streak_day":false}}
//	→ {"code":10001,"msg":"今天已签到，请明天再来"}
//
// origin：国内 workbuddy → https://copilot.tencent.com，国际 → https://www.workbuddy.ai
func (s *Server) handleWorkBuddyCheckin(w http.ResponseWriter, r *http.Request) {
	dry := r.Method == http.MethodGet
	if !dry && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST 签到 / GET 查状态"})
		return
	}

	cfg := s.store.Snapshot()
	// 允许 body 里指定 provider / accountId
	var opt struct {
		Provider  string `json:"provider"`
		AccountID string `json:"accountId"`
	}
	if !dry && r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&opt)
	}
	pid := strings.TrimSpace(opt.Provider)
	if pid == "" {
		pid = r.URL.Query().Get("provider")
	}

	targets := []string{}
	if pid != "" {
		targets = append(targets, pid)
	} else {
		for _, p := range cfg.Providers {
			if p != nil && (p.ID == "workbuddy" || p.ID == "workbuddy-ai") {
				targets = append(targets, p.ID)
			}
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "total": 0, "succeeded": 0, "already": 0, "failed": 0,
			"results": []any{},
			"note":    "配置里没有 workbuddy / workbuddy-ai 供应商",
		})
		return
	}

	var results []map[string]any
	var ok, already, failed, skipped int
	changed := false

	for _, target := range targets {
		p := cfg.FindProvider(target)
		if p == nil {
			results = append(results, map[string]any{"provider": target, "ok": false, "error": "供应商不存在"})
			failed++
			continue
		}
		origin := "https://copilot.tencent.com"
		if p.ID == "workbuddy-ai" {
			origin = "https://www.workbuddy.ai"
		}
		if p.BaseURL != "" {
			// baseUrl 一般带 /v1，签到路径在根上
			origin = strings.TrimSuffix(strings.TrimSuffix(p.BaseURL, "/v1"), "/")
		}

		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			row := map[string]any{"provider": p.ID, "accountId": a.ID, "name": a.Name}
			if !a.IsEnabled() {
				row["ok"] = false
				row["skipped"] = "disabled"
				results = append(results, row)
				skipped++
				continue
			}
			if a.UID == "" || a.Key() == "" {
				row["ok"] = false
				row["skipped"] = "no_credential"
				results = append(results, row)
				skipped++
				continue
			}
			if opt.AccountID != "" && a.ID != opt.AccountID {
				continue
			}
			if dry {
				row["ok"] = true
				row["checkinDate"] = a.CheckinDate
				row["checkinOutcome"] = a.CheckinOutcome
				results = append(results, row)
				continue
			}

			credit, streak, outcome, err := s.doCheckin(origin, a)
			row["outcome"] = outcome
			if err != nil {
				row["ok"] = false
				row["error"] = err.Error()
				failed++
			} else {
				row["ok"] = true
				row["credit"] = credit
				row["streakDays"] = streak
				if outcome == "already" {
					already++
				} else {
					ok++
				}
				// 回填签到记录（已签到也记，避免下次白打一次上游）
				a.CheckinDate = today()
				a.CheckinOutcome = outcome
				a.CheckinCredit = credit
				a.CheckinStreak = streak
				changed = true
			}
			results = append(results, row)
			time.Sleep(1200 * time.Millisecond) // 节流，避免连续请求被风控
		}
	}

	if changed {
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "error": "签到已完成但配置落盘失败: " + err.Error(), "results": results,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        failed == 0,
		"dryRun":    dry,
		"total":     len(results),
		"succeeded": ok,
		"already":   already,
		"failed":    failed,
		"skipped":   skipped,
		"results":   results,
	})
}

// doCheckin 对单个账号执行签到。
func (s *Server) doCheckin(origin string, a *config.Account) (credit, streak int, outcome string, err error) {
	url := strings.TrimRight(origin, "/") + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return 0, 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.Key())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("X-User-Id", a.UID)
	domain := a.Domain
	if domain == "" {
		domain = "copilot.tencent.com"
	}
	req.Header.Set("X-Domain", domain)
	req.Header.Set("Accept-Language", "zh-CN")

	client := &http.Client{Timeout: 40 * time.Second, Transport: provider.HTTPClient.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, "network", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit     int  `json:"credit"`
			StreakDays int  `json:"streak_days"`
			IsStreak   bool `json:"is_streak_day"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return 0, 0, "sign_in_required", fmt.Errorf("凭据失效（HTTP %d）", resp.StatusCode)
		}
		return 0, 0, "bad_response", fmt.Errorf("上游返回非 JSON（HTTP %d）", resp.StatusCode)
	}
	if env.Code == 0 {
		return env.Data.Credit, env.Data.StreakDays, "ok", nil
	}
	if strings.Contains(env.Msg, "已签到") || strings.Contains(strings.ToLower(env.Msg), "already") {
		return env.Data.Credit, env.Data.StreakDays, "already", nil
	}
	return 0, 0, "rejected", fmt.Errorf("%s", env.Msg)
}

func today() string {
	return time.Now().Format("2006-01-02")
}

// mergeMaskedSecrets 把 incoming 中被掩码（或留空）的敏感字段还原成 cur 里的真实值。
//
// 背景：GET /api/config 返回的是脱敏视图（apiKey / access / refresh 变成 "abc123...wxyz"），
// 而 Store.Save 是整份替换。面板在「设置」里只改端口/口令时会把整份视图回传，
// 若不还原，密钥就会被写成掩码串，等于把凭据弄坏。留空同样按"不修改"处理，
// 与 Server.AuthKey 的既有语义保持一致。
func mergeMaskedSecrets(cur, incoming *config.Config) {
	if incoming == nil || cur == nil {
		return
	}
	for _, p := range incoming.Providers {
		if p == nil {
			continue
		}
		old := cur.FindProvider(p.ID)
		if old == nil {
			continue
		}
		if maskedOrEmpty(p.APIKey) {
			p.APIKey = old.APIKey
		}
		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			var oa *config.Account
			for _, x := range old.Accounts {
				if x != nil && x.ID == a.ID {
					oa = x
					break
				}
			}
			if oa == nil {
				continue
			}
			if maskedOrEmpty(a.Access) {
				a.Access = oa.Access
			}
			if maskedOrEmpty(a.Refresh) {
				a.Refresh = oa.Refresh
			}
		}
	}
}

// maskedOrEmpty 判断一个字段值是不是"脱敏串或空"，这两种都表示调用方没打算改它。
func maskedOrEmpty(v string) bool {
	return v == "" || strings.Contains(v, "...")
}

// handleAccounts 导出账号池的健康状态（成功数、失败数、冷却剩余）。
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accounts": s.pool.Snapshot(),
	})
}
