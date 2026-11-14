package gateway

import (
	"context"
	"net/http"
	"sync"
	"time"

	"allin/internal/config"
	"allin/internal/provider/workbuddy"
)

var (
	wbFlowsMu sync.Mutex
	wbFlows   = map[string]*workbuddy.LoginFlow{}
)

// handleWorkBuddyStart 申请一个 WorkBuddy 登录态，返回授权地址。
//
//	POST /api/auth/workbuddy/start  {"provider":"workbuddy"}
func (s *Server) handleWorkBuddyStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	_ = decodeJSON(r, &body)
	pid := body.Provider
	if pid == "" {
		pid = "workbuddy"
	}
	if pid != "workbuddy" && pid != "workbuddy-ai" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "provider 只能是 workbuddy 或 workbuddy-ai"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	flow, err := workbuddy.StartLogin(ctx, pid)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	wbFlowsMu.Lock()
	wbFlows[flow.State] = flow
	wbFlowsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"flowId":   flow.State,
		"url":      flow.AuthURL,
		"provider": pid,
		"deadline": flow.Deadline.Unix(),
		"hint":     "在浏览器打开 url 用手机号登录 WorkBuddy，然后轮询 poll",
	})
}

// handleWorkBuddyPoll 轮询登录结果，成功后把账号写入配置。
//
//	GET /api/auth/workbuddy/poll?flowId=<state>
func (s *Server) handleWorkBuddyPoll(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flowId")
	if flowID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 flowId"})
		return
	}
	wbFlowsMu.Lock()
	flow := wbFlows[flowID]
	wbFlowsMu.Unlock()
	if flow == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "登录流不存在或已过期"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	acc, err := workbuddy.PollLogin(ctx, flow)
	if err != nil {
		if err == workbuddy.ErrPending {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "pending"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": "failed", "error": err.Error()})
		return
	}

	wbFlowsMu.Lock()
	delete(wbFlows, flowID)
	wbFlowsMu.Unlock()

	// 写入配置
	cfg := s.store.Snapshot()
	prov := cfg.FindProvider(flow.Provider)
	if prov == nil {
		prov = &config.Provider{
			ID: flow.Provider, Name: "WorkBuddy", Kind: "workbuddy",
			BaseURL: "https://copilot.tencent.com/v1",
			Models:  []string{"glm-5.3", "deepseek-v4.1-flash", "hy3"},
		}
		if flow.Provider == "workbuddy-ai" {
			prov.Name = "WorkBuddy 国际版"
			prov.BaseURL = "https://www.workbuddy.ai/v1"
		}
		cfg.Providers = append(cfg.Providers, prov)
	}
	var replaced bool
	for i, a := range prov.Accounts {
		if a != nil && a.UID == acc.UID {
			prov.Accounts[i] = wbToConfig(acc)
			replaced = true
			break
		}
	}
	if !replaced {
		prov.Accounts = append(prov.Accounts, wbToConfig(acc))
	}
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "登录成功但保存失败: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"status":   "done",
		"account":  acc.Name,
		"provider": flow.Provider,
		"hint":     "已写入配置，模型名可用 " + flow.Provider + "/glm-5.3",
	})
}

func wbToConfig(a *workbuddy.LoginAccount) *config.Account {
	enabled := true
	return &config.Account{
		ID:      "wb-" + a.UID,
		Name:    a.Name,
		Type:    "token",
		Enabled: &enabled,
		Access:  a.Access,
		Refresh: a.Refresh,
		UID:     a.UID,
		Domain:  a.Domain,
	}
}
