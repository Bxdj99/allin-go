package gateway

import (
	"net/http"
	"strings"
	"time"

	"allin/internal/config"
)

// 账号 JSON 导入/导出。
//
// 对齐上游 1.2.41 的「账号 JSON 文件」功能：一个文件装多个供应商的多个账号，
// 方便换机迁移、朋友之间分享账号池（文件本身含凭据，别外传）。
//
// 格式（与 config.json 里的 providers.accounts 同构）：
//
//	{
//	  "exportedAt": 1794690000,
//	  "version": 1,
//	  "accounts": [
//	    {"providerId": "workbuddy", "id": "...", "name": "...", "access": "...", "uid": "..."}
//	  ]
//	}

// handleAccountsExport 导出全部账号（或指定供应商的）。
//
//	GET /api/accounts/export?providerId=可选
func (s *Server) handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	want := strings.TrimSpace(r.URL.Query().Get("providerId"))

	type row struct {
		ProviderID string `json:"providerId"`
		ID         string `json:"id"`
		Name       string `json:"name,omitempty"`
		Type       string `json:"type,omitempty"`
		Access     string `json:"access,omitempty"`
		Refresh    string `json:"refresh,omitempty"`
		UID        string `json:"uid,omitempty"`
		Domain     string `json:"domain,omitempty"`
		Realm      string `json:"realm,omitempty"`
		DeviceID   string `json:"deviceId,omitempty"`
		MachineID  string `json:"machineId,omitempty"`
		PublicKey  string `json:"publicKey,omitempty"`
		PrivateKey string `json:"privateKey,omitempty"`
		Enabled    *bool  `json:"enabled,omitempty"`
	}
	var rows []row
	for _, p := range cfg.Providers {
		if p == nil {
			continue
		}
		if want != "" && p.ID != want {
			continue
		}
		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			rows = append(rows, row{
				ProviderID: p.ID, ID: a.ID, Name: a.Name, Type: a.Type,
				Access: a.Access, Refresh: a.Refresh, UID: a.UID,
				Domain: a.Domain, Realm: a.Realm,
				DeviceID: a.DeviceID, MachineID: a.MachineID,
				PublicKey: a.PublicKey, PrivateKey: a.PrivateKey,
				Enabled: a.Enabled,
			})
		}
	}
	// 没有 accounts 字段时也导出 provider 级别的 apiKey
	type providerKeys struct {
		ProviderID string `json:"providerId"`
		APIKey     string `json:"apiKey,omitempty"`
	}
	var keys []providerKeys
	for _, p := range cfg.Providers {
		if p == nil || p.APIKey == "" {
			continue
		}
		if want != "" && p.ID != want {
			continue
		}
		keys = append(keys, providerKeys{ProviderID: p.ID, APIKey: p.APIKey})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"version":    1,
		"exportedAt": time.Now().Unix(),
		"accounts":   rows,
		"providerKeys": keys,
		"note":       "文件包含凭据，不要外传",
	})
}

// handleAccountsImport 导入账号。
//
//	POST /api/accounts/import  {"accounts":[...], "providerKeys":[...], "mode":"merge"}
//
// mode:
//
//	merge  与现有账号合并（同 ID 覆盖，无则追加）——默认
//	skip   同 ID 已存在就跳过
func (s *Server) handleAccountsImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var in struct {
		Mode          string `json:"mode"`
		Accounts      []struct {
			ProviderID string `json:"providerId"`
			ID         string `json:"id"`
			Name       string `json:"name"`
			Type       string `json:"type"`
			Access     string `json:"access"`
			Refresh    string `json:"refresh"`
			UID        string `json:"uid"`
			Domain     string `json:"domain"`
			Realm      string `json:"realm"`
			DeviceID   string `json:"deviceId"`
			MachineID  string `json:"machineId"`
			PublicKey  string `json:"publicKey"`
			PrivateKey string `json:"privateKey"`
			Enabled    *bool  `json:"enabled"`
		} `json:"accounts"`
		ProviderKeys []struct {
			ProviderID string `json:"providerId"`
			APIKey     string `json:"apiKey"`
		} `json:"providerKeys"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
		return
	}
	mode := strings.TrimSpace(in.Mode)
	if mode == "" {
		mode = "merge"
	}
	if mode != "merge" && mode != "skip" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "mode 只能是 merge / skip"})
		return
	}

	cfg := s.store.Snapshot()
	added, updated, skipped := 0, 0, 0

	for _, na := range in.Accounts {
		if strings.TrimSpace(na.ProviderID) == "" || strings.TrimSpace(na.Access) == "" {
			skipped++
			continue
		}
		p := cfg.FindProvider(na.ProviderID)
		if p == nil {
			// 供应商不存在就建一个（kind 按 id 猜一个合理值）
			p = &config.Provider{
				ID: na.ProviderID, Name: na.ProviderID,
				Kind: guessKind(na.ProviderID),
			}
			cfg.Providers = append(cfg.Providers, p)
		}
		id := strings.TrimSpace(na.ID)
		if id == "" {
			id = p.ID + "-" + shortID()
		}
		// 找现有
		var target *config.Account
		for _, a := range p.Accounts {
			if a != nil && a.ID == id {
				target = a
				break
			}
		}
		if target != nil {
			if mode == "skip" {
				skipped++
				continue
			}
			updated++
		} else {
			target = &config.Account{ID: id, AddedAt: time.Now().UnixMilli()}
			p.Accounts = append(p.Accounts, target)
			added++
		}
		// 覆盖字段（空值不动原有）
		if na.Name != "" {
			target.Name = na.Name
		}
		if na.Type != "" {
			target.Type = na.Type
		}
		if na.Access != "" {
			target.Access = na.Access
		}
		if na.Refresh != "" {
			target.Refresh = na.Refresh
		}
		if na.UID != "" {
			target.UID = na.UID
		}
		if na.Domain != "" {
			target.Domain = na.Domain
		}
		if na.Realm != "" {
			target.Realm = na.Realm
		}
		if na.DeviceID != "" {
			target.DeviceID = na.DeviceID
		}
		if na.MachineID != "" {
			target.MachineID = na.MachineID
		}
		if na.PublicKey != "" {
			target.PublicKey = na.PublicKey
		}
		if na.PrivateKey != "" {
			target.PrivateKey = na.PrivateKey
		}
		if na.Enabled != nil {
			target.Enabled = na.Enabled
		}
	}

	// provider 级 key
	for _, nk := range in.ProviderKeys {
		if strings.TrimSpace(nk.ProviderID) == "" || strings.TrimSpace(nk.APIKey) == "" {
			continue
		}
		p := cfg.FindProvider(nk.ProviderID)
		if p == nil {
			p = &config.Provider{ID: nk.ProviderID, Name: nk.ProviderID, Kind: guessKind(nk.ProviderID)}
			cfg.Providers = append(cfg.Providers, p)
		}
		p.APIKey = nk.APIKey
	}

	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "added": added, "updated": updated, "skipped": skipped,
		"hint": "已写入配置，立即生效",
	})
}

// guessKind 按供应商 id 猜协议类型（导入时遇到未注册的供应商用）。
func guessKind(id string) string {
	switch {
	case strings.Contains(id, "workbuddy"):
		return "workbuddy"
	case strings.Contains(id, "trae"):
		return "trae"
	case strings.Contains(id, "copilot"):
		return "copilot"
	case strings.Contains(id, "codex"):
		return "codex"
	case strings.Contains(id, "deepseek") && strings.Contains(id, "web"):
		return "deepseek-web"
	case strings.Contains(id, "qoder"):
		return "qoder"
	case strings.Contains(id, "raccoon"):
		return "raccoon"
	case strings.Contains(id, "loomy"):
		return "loomy"
	case strings.Contains(id, "cline"):
		return "cline"
	case strings.Contains(id, "minimax"):
		return "minimax"
	case strings.Contains(id, "catpaw"):
		return "catpaw"
	case strings.Contains(id, "wenxin"):
		return "wenxin"
	case strings.Contains(id, "antigravity"):
		return "antigravity"
	case strings.Contains(id, "anthropic"):
		return "anthropic"
	default:
		return "openai"
	}
}
