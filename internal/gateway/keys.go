package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"allin/internal/config"
)

// handleKeys 列出 / 新建 / 删除客户端 API Key。
//
//	GET    /api/keys                 列出（key 脱敏）
//	POST   /api/keys  {"note":"手机"}  新建，返回明文（只此一次）
//	DELETE /api/keys?id=xxx          删除
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()

	switch r.Method {
	case http.MethodGet:
		out := make([]map[string]any, 0, len(cfg.APIKeys))
		for _, k := range cfg.APIKeys {
			if k == nil {
				continue
			}
			row := map[string]any{
				"id":        k.ID,
				"note":      k.Note,
				"preview":   maskKey(k.Key),
				"enabled":   k.IsEnabled(),
				"createdAt": k.CreatedAt,
			}
			// 主口令也列出来，方便用户知道有哪几把能用
			if r.URL.Query().Get("withPrimary") != "" {
				row["primary"] = false
			}
			out = append(out, row)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"keys":     out,
			"primary":  maskKey(cfg.Server.AuthKey),
			"endpoint": endpointOf(r),
		})
		return

	case http.MethodPost:
		var body struct {
			Note string `json:"note"`
		}
		_ = decodeJSON(r, &body)

		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "随机数不可用"})
			return
		}
		plain := "sk-allin-" + hex.EncodeToString(raw)
		enabled := true
		id := "key-" + hex.EncodeToString(raw[:6])
		cfg.APIKeys = append(cfg.APIKeys, &config.APIKey{
			ID:        id,
			Note:      strings.TrimSpace(body.Note),
			Key:       plain,
			Enabled:   &enabled,
			CreatedAt: time.Now().UnixMilli(),
		})
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		// 明文只在这里返回一次，列表里永远只给掩码
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":   true,
			"id":   id,
			"key":  plain,
			"note": body.Note,
			"hint": "这串 key 只显示这一次，请现在复制保存",
		})
		return

	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 id"})
			return
		}
		kept := make([]*config.APIKey, 0, len(cfg.APIKeys))
		found := false
		for _, k := range cfg.APIKeys {
			if k != nil && k.ID == id {
				found = true
				continue
			}
			kept = append(kept, k)
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "key 不存在"})
			return
		}
		cfg.APIKeys = kept
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST/DELETE"})
}

// handleKeyToggle 启用/禁用一把 key。
//
//	POST /api/keys/toggle  {"id":"key-xxx","enabled":false}
func (s *Server) handleKeyToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败"})
		return
	}
	cfg := s.store.Snapshot()
	var target *config.APIKey
	for _, k := range cfg.APIKeys {
		if k != nil && k.ID == body.ID {
			target = k
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "key 不存在"})
		return
	}
	en := body.Enabled
	target.Enabled = &en
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": body.ID, "enabled": body.Enabled})
}

// endpointOf 推导客户端应该填的 Base URL。
func endpointOf(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = "127.0.0.1:3425"
	}
	return "http://" + host + "/v1"
}

// keyAllowed 判断请求携带的 key 是否有效（主口令或任意启用的 API Key）。
func keyAllowed(cfg *config.Config, got string) bool {
	if got == "" {
		return false
	}
	if cfg.Server.AuthKey != "" && got == cfg.Server.AuthKey {
		return true
	}
	for _, k := range cfg.APIKeys {
		if k != nil && k.IsEnabled() && k.Key != "" && k.Key == got {
			return true
		}
	}
	return false
}
