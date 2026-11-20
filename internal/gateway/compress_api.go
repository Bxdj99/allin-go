package gateway

import "net/http"

// 历史压缩设置。
//
//	GET  /api/compress   读当前设置
//	POST /api/compress   改设置 {"enabled":true|false, "keepRecent":6}

// handleCompressGet 返回当前压缩设置。
func (s *Server) handleCompressGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"enabled":    cfg.Server.HistoryCompressEnabled(),
		"keepRecent": cfg.Server.CompressKeepRecentGroups(),
		"maxOutput":  cfg.Server.MaxOutputCap(),
		"note":       "开启会把较早的工具调用摘要掉，减小请求体；但会改变 prompt 前缀，可能降低缓存命中率",
	})
}

// handleCompressSet 改压缩设置。
func (s *Server) handleCompressSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var in struct {
		Enabled    *bool `json:"enabled"`
		KeepRecent *int  `json:"keepRecent"`
		MaxOutput  *int  `json:"maxOutput"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
		return
	}
	if in.Enabled == nil && in.KeepRecent == nil && in.MaxOutput == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "需要 enabled / keepRecent / maxOutput 之一"})
		return
	}
	if in.KeepRecent != nil && (*in.KeepRecent < 0 || *in.KeepRecent > 200) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "keepRecent 需在 0-200 之间"})
		return
	}
	if in.MaxOutput != nil && (*in.MaxOutput < 0 || *in.MaxOutput > 2_000_000) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "maxOutput 需在 0-2000000 之间"})
		return
	}

	cfg := s.store.Snapshot()
	if in.Enabled != nil {
		v := *in.Enabled
		cfg.Server.HistoryCompress = &v
	}
	if in.KeepRecent != nil {
		v := *in.KeepRecent
		cfg.Server.CompressKeepRecent = &v
	}
	if in.MaxOutput != nil {
		v := *in.MaxOutput
		cfg.Server.MaxOutputTokens = &v
	}
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"enabled":    cfg.Server.HistoryCompressEnabled(),
		"keepRecent": cfg.Server.CompressKeepRecentGroups(),
		"maxOutput":  cfg.Server.MaxOutputCap(),
	})
}
