package gateway

import (
	"log"
	"net/http"
	"strconv"
	"strings"
)

// 日志页接口。
//
//	GET  /api/logs?limit=100   读最近 N 条采样 + 汇总
//	POST /api/logs/toggle      开关采样 {"enabled":true|false}
//	POST /api/logs/clear       清空已采集的采样
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	enabled := cfg.Server.LogCaptureEnabled()

	limit := 100
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > ttftCapacity {
		limit = ttftCapacity
	}

	sum := s.ttft.Summary(limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"enabled": enabled,
		"capacity": ttftCapacity,
		"summary": sum,
	})
}

// handleLogsToggle 开关采样。关闭时一并清空已采集的数据，
// 免得界面关掉后还残留旧样本误导人。
func (s *Server) handleLogsToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "需要 enabled 字段（true/false）"})
		return
	}

	cfg := s.store.Snapshot()
	v := *in.Enabled
	cfg.Server.LogCapture = &v
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	// 总开关：同时管住 stdout 日志与采样缓冲
	SetLogEnabled(v)
	if !v {
		s.ttft.Clear()
	}
	log.Printf("日志总开关：%v", v)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": v})
}

// handleLogsClear 只清空采样，不动开关。
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	s.ttft.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
