package gateway

import (
	"net/http"
	"strings"

	"allin/internal/config"
	"allin/internal/prompt"
)

// handlePrompt 读写网关级系统提示词。
//
//	GET  /api/prompt  → {mode, inline, file, preview, length}
//	POST /api/prompt  {mode, inline, file}   保存
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()

	switch r.Method {
	case http.MethodGet:
		text := ""
		if cfg.Prompt.Inline != "" {
			text = cfg.Prompt.Inline
		} else if cfg.Prompt.File != "" {
			loader := prompt.Config{Mode: prompt.ModeInject, File: cfg.Prompt.File}
			if loaded, err := loader.Load(); err == nil {
				text = loaded
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"mode":   cfg.Prompt.Mode,
			"inline": text,
			"file":   cfg.Prompt.File,
			"length": len(text),
			"modes": []map[string]string{
				{"value": "passthrough", "label": "passthrough — 原样转发，不注入"},
				{"value": "inject", "label": "inject — 插在客户端提示词前面（压过它）"},
				{"value": "replace", "label": "replace — 丢掉客户端自带的，只用自己的"},
			},
		})
		return

	case http.MethodPost:
		var body struct {
			Mode   string `json:"mode"`
			Inline string `json:"inline"`
			File   string `json:"file"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
			return
		}
		mode := strings.TrimSpace(body.Mode)
		switch mode {
		case prompt.ModePassthrough, prompt.ModeInject, prompt.ModeReplace:
		case "":
			mode = prompt.ModePassthrough
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "mode 只能是 passthrough / inject / replace"})
			return
		}
		if mode != prompt.ModePassthrough && body.Inline == "" && body.File == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "选了 " + mode + " 就必须填提示词正文或文件路径"})
			return
		}
		cfg.Prompt = config.PromptConfig{
			Mode:   mode,
			Inline: body.Inline,
			File:   strings.TrimSpace(body.File),
		}
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"mode":   mode,
			"length": len(body.Inline),
			"hint":   "已保存，下一个请求立即生效（无需重启）",
		})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
}

// handlePromptPreset 一键写入内置人设（目前提供 M0SS 那份）。
//
//	POST /api/prompt/preset  {"name":"m0ss"}
func (s *Server) handlePromptPreset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = decodeJSON(r, &body)
	if body.Name != "m0ss" && body.Name != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "未知预设: " + body.Name})
		return
	}
	cfg := s.store.Snapshot()
	cfg.Prompt = config.PromptConfig{Mode: prompt.ModeInject, Inline: M0SSPrompt}
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": prompt.ModeInject, "length": len(M0SSPrompt)})
}
