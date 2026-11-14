package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"allin/internal/config"
	"allin/internal/provider/copilot"
)

// copilotFlows 保存进行中的 GitHub 设备码登录流。
var (
	copilotFlowsMu sync.Mutex
	copilotFlows   = map[string]*copilot.DeviceFlow{}
)

// handleCopilotStart 启动 GitHub 设备码登录。
//
//	POST /api/auth/github-copilot/start
//	→ {"flowId":"...","userCode":"7C39-BAC5","verificationUri":"https://github.com/login/device"}
func (s *Server) handleCopilotStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	flow, err := copilot.StartDeviceFlow(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 用 userCode 当 flowId 的索引键（设备码本身唯一）
	key := flow.UserCode
	copilotFlowsMu.Lock()
	copilotFlows[key] = flow
	copilotFlowsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"flowId":          key,
		"userCode":        flow.UserCode,
		"verificationUri": flow.VerificationURI,
		"intervalMs":      flow.IntervalMs,
		"verificationUriComplete": flow.VerificationURIComplete,
		"expiresInSec":    flow.ExpiresIn,
		"hint":            "浏览器打开 verificationUri，输入 userCode 完成授权，然后轮询 poll",
	})
}

// handleCopilotPoll 轮询设备码授权结果；成功后写入 github-copilot 供应商。
//
//	POST /api/auth/github-copilot/poll  {"flowId":"7C39-BAC5"}
func (s *Server) handleCopilotPoll(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flowId")
	if flowID == "" && r.Body != nil {
		var body struct {
			FlowID string `json:"flowId"`
		}
		if err := decodeJSON(r, &body); err == nil {
			flowID = body.FlowID
		}
	}
	if flowID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 flowId"})
		return
	}

	copilotFlowsMu.Lock()
	flow := copilotFlows[flowID]
	copilotFlowsMu.Unlock()
	if flow == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "登录流不存在或已过期"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	acc, err := copilot.PollDeviceFlow(ctx, flow)
	if err != nil {
		if err == copilot.ErrAuthorizationPending {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "pending"})
			return
		}
		if err == copilot.ErrSlowDown {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "slow_down"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "status": "failed", "error": err.Error()})
		return
	}

	copilotFlowsMu.Lock()
	delete(copilotFlows, flowID)
	copilotFlowsMu.Unlock()

	// 写入配置
	cfg := s.store.Snapshot()
	prov := cfg.FindProvider("github-copilot")
	if prov == nil {
		prov = &config.Provider{
			ID: "github-copilot", Name: "GitHub Copilot", Kind: "copilot",
			BaseURL: "https://api.githubcopilot.com",
			Models:  copilot.DefaultModels,
		}
		cfg.Providers = append(cfg.Providers, prov)
	}
	// 同一 GitHub 账号重复登录就覆盖（用 refresh 里的 GitHub token 判断不了身份，
	// 用 Name 兜底：同名视为同账号）
	var replaced bool
	for i, a := range prov.Accounts {
		if a != nil && a.Name != "" && a.Name == acc.Name {
			prov.Accounts[i] = copilotToConfig(acc)
			replaced = true
			break
		}
	}
	if !replaced {
		prov.Accounts = append(prov.Accounts, copilotToConfig(acc))
	}
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "登录成功但保存配置失败: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"status":   "done",
		"account":  acc.Name,
		"provider": "github-copilot",
		"hint":     "已写入配置，模型名可用 github-copilot/<模型>",
	})
}

func copilotToConfig(a *copilot.Account) *config.Account {
	enabled := true
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = "copilot"
	}
	return &config.Account{
		ID:      "copilot-" + name,
		Name:    name,
		Type:    "oauth",
		Enabled: &enabled,
		Access:  a.Access,
		Refresh: a.Refresh,
		Expires: a.Expires,
		UID:     a.Name,
	}
}
