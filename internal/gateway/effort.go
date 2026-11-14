package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"allin/internal/config"
	"allin/internal/prompt"
)

// 思考强度（reasoning effort）相关的读写与解析。
//
// 背景：各家上游的思考参数名不统一 ——
//   OpenAI 系 / WorkBuddy / Trae：reasoning_effort: low|medium|high|xhigh|max
//   Anthropic：thinking: {type:"enabled", budget_tokens:N}
//   DeepSeek 网页版：thinking_enabled: true/false
// 网关统一用「档位」这一层抽象，再按供应商协议翻译。

// EffortLevels 是面板下拉里出现的档位。
var EffortLevels = []map[string]string{
	{"value": "", "label": "跟随请求（客户端发什么就是什么）"},
	{"value": "off", "label": "关闭思考"},
	{"value": "low", "label": "低 —— 快，省 token"},
	{"value": "medium", "label": "中"},
	{"value": "high", "label": "高 —— 默认推荐"},
	{"value": "xhigh", "label": "极高"},
	{"value": "max", "label": "拉满 —— 最慢最贵"},
}

// handleEffort 读取或保存全局思考强度默认值。
//
//	GET  /api/effort  → {mode, effort, levels}
//	POST /api/effort  {mode, effort}
//
// mode:
//
//	auto     客户端带了就用客户端的；没带就用 effort 指定值
//	force    无条件用 effort 指定值，覆盖客户端
func (s *Server) handleEffort(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"mode":   cfg.Reasoning.Mode,
			"effort": cfg.Reasoning.Effort,
			"levels": EffortLevels,
			"hint":   "客户端也能在模型名后面加后缀覆盖，例如 workbuddy/glm-5.3:high",
		})
		return

	case http.MethodPost:
		var body struct {
			Mode   string `json:"mode"`
			Effort string `json:"effort"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败"})
			return
		}
		mode := strings.TrimSpace(body.Mode)
		switch mode {
		case "", "auto":
			mode = "auto"
		case "force":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "mode 只能是 auto / force"})
			return
		}
		effort := strings.TrimSpace(body.Effort)
		if !validEffort(effort) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "effort 取值不对: " + effort})
			return
		}
		cfg.Reasoning = config.ReasoningConfig{Mode: mode, Effort: effort}
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode, "effort": effort})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
}

func validEffort(e string) bool {
	switch e {
	case "", "off", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

// splitModelEffort 从模型名里拆出后缀档位：workbuddy/glm-5.3:high → (workbuddy/glm-5.3, high)
func splitModelEffort(model string) (string, string) {
	i := strings.LastIndexByte(model, ':')
	if i <= 0 || i == len(model)-1 {
		return model, ""
	}
	suffix := model[i+1:]
	if !validEffort(suffix) {
		return model, ""
	}
	return model[:i], suffix
}

// resolveEffort 决定本次请求实际使用的档位。
//
// 优先级：模型名后缀 > 客户端请求体 > 全局配置（force 模式下压过客户端）
func resolveEffort(cfg *config.Config, modelSuffix string, body map[string]json.RawMessage) string {
	// 1) 模型名后缀最优先
	if modelSuffix != "" {
		return modelSuffix
	}
	// 2) force 模式无条件用全局值
	if cfg.Reasoning.Mode == "force" && cfg.Reasoning.Effort != "" {
		return cfg.Reasoning.Effort
	}
	// 3) 客户端自带的
	if raw, ok := body["reasoning_effort"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && validEffort(v) {
			return v
		}
	}
	if raw, ok := body["reasoning"]; ok {
		var r map[string]json.RawMessage
		if json.Unmarshal(raw, &r) == nil {
			var v string
			if json.Unmarshal(r["effort"], &v) == nil && validEffort(v) {
				return v
			}
		}
	}
	// 4) auto 模式下的全局默认
	if cfg.Reasoning.Mode == "auto" && cfg.Reasoning.Effort != "" {
		return cfg.Reasoning.Effort
	}
	return ""
}

// applyEffortToBody 把档位写进请求体（按上游协议不同）。
//
// upstreamIsAnthropic 为真时写 thinking.budget_tokens，否则写 reasoning_effort。
func applyEffortToBody(body []byte, effort string, upstreamIsAnthropic bool) ([]byte, error) {
	if effort == "" {
		return body, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, err
	}

	if upstreamIsAnthropic {
		if effort == "off" {
			m["thinking"] = json.RawMessage(`{"type":"disabled"}`)
		} else {
			budget := 4096
			switch effort {
			case "low":
				budget = 1024
			case "medium":
				budget = 4096
			case "high":
				budget = 16384
			case "xhigh":
				budget = 32768
			case "max":
				budget = 65536
			}
			v, _ := json.Marshal(map[string]any{"type": "enabled", "budget_tokens": budget})
			m["thinking"] = v
		}
	} else {
		if effort == "off" {
			// OpenAI 系没有统一的「关」字段，删掉即可
			delete(m, "reasoning_effort")
			delete(m, "reasoning")
		} else {
			v, _ := json.Marshal(effort)
			m["reasoning_effort"] = v
		}
	}
	return json.Marshal(m)
}

// ensure prompt 包被引用（handlePrompt 在另一个文件里用）
var _ = prompt.ModeInject
