package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"allin/internal/config"
	"allin/internal/provider/trae"
)

// traeFlows 保存进行中的 Trae 登录流（按 flowId 索引）。
var (
	traeFlowsMu sync.Mutex
	traeFlows   = map[string]*trae.Flow{}
)

// handleTraeStart 启动一个 Trae 登录流，返回需要用户打开的授权 URL。
//
//	POST /api/auth/trae/start
//	→ {"flowId":"...","url":"https://www.trae.cn/authorization?...","deadline":...}
func (s *Server) handleTraeStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	// 清理过期流
	traeFlowsMu.Lock()
	for id, f := range traeFlows {
		if st := f.Status(); st["status"] != "pending" {
			delete(traeFlows, id)
		}
	}
	traeFlowsMu.Unlock()

	flow, err := trae.StartLogin(5 * time.Minute)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	traeFlowsMu.Lock()
	traeFlows[flow.FlowID] = flow
	traeFlowsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"flowId":   flow.FlowID,
		"url":      flow.AuthURL,
		"deadline": flow.Deadline.Unix(),
		"hint":     "在浏览器打开 url 完成 Trae 授权，然后轮询 /api/auth/trae/poll?flowId=...",
	})
}

// handleTraePoll 查询登录状态；成功后把账号写入 trae 供应商的 accounts。
//
//	GET /api/auth/trae/poll?flowId=xxx
func (s *Server) handleTraePoll(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flowId")
	if flowID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 flowId"})
		return
	}
	traeFlowsMu.Lock()
	flow := traeFlows[flowID]
	traeFlowsMu.Unlock()
	if flow == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "登录流不存在或已过期"})
		return
	}

	st := flow.Status()
	status, _ := st["status"].(string)
	if status != "done" {
		writeJSON(w, http.StatusOK, st)
		return
	}

	acc := flow.TakeResult()
	if acc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "done", "note": "账号已被取走"})
		return
	}
	defer flow.Close()

	// 写入配置：trae 供应商的 accounts
	cfg := s.store.Snapshot()
	prov := cfg.FindProvider("trae-cn")
	if prov == nil {
		prov = &config.Provider{
			ID: "trae-cn", Name: "Trae CN", Kind: "trae",
			BaseURL: "https://trae-api-cn.mchost.guru",
			Models:  []string{"claude-3.5-sonnet", "gpt-4o"},
		}
		cfg.Providers = append(cfg.Providers, prov)
	}
	// 同一设备重复登录就覆盖
	var replaced bool
	for i, a := range prov.Accounts {
		if a != nil && a.DeviceID == acc.DeviceID {
			prov.Accounts[i] = traeAccount(acc)
			replaced = true
			break
		}
	}
	if !replaced {
		prov.Accounts = append(prov.Accounts, traeAccount(acc))
	}
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "登录成功但保存配置失败: " + err.Error()})
		return
	}

	name := acc.Name
	if name == "" {
		name = acc.UID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "done",
		"account": name,
		"provider": "trae-cn",
		"hint":    "已写入配置，模型名可用 trae-cn/<模型>",
	})
}

func traeAccount(a *trae.Account) *config.Account {
	enabled := true
	return &config.Account{
		ID:         "trae-" + strings.TrimSpace(a.DeviceID),
		Name:       a.Name,
		Type:       "token",
		Enabled:    &enabled,
		Access:     a.Token,
		Refresh:    a.Refresh,
		UID:        a.UID,
		Expires:    a.Expires,
		DeviceID:   a.DeviceID,
		MachineID:  a.MachineID,
		PublicKey:  a.PublicKey,
		PrivateKey: a.PrivateKey,
	}
}

// ensure fmt 被使用（错误信息拼装）
var _ = fmt.Sprintf
