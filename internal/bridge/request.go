package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAIToAnthropicRequest 把 OpenAI Chat Completions 请求体转成 Anthropic Messages 请求体。
//
// 主要差异：
//   - OpenAI 的 system 是 messages 里的一条；Anthropic 是顶层 system 字段
//   - OpenAI 的 tool_calls 挂在 assistant 消息上；Anthropic 是 content 里的 tool_use block
//   - OpenAI 的 role=tool 是独立消息；Anthropic 是 user 消息里的 tool_result block
//   - Anthropic 的 max_tokens 必填，缺省时补 4096
func OpenAIToAnthropicRequest(in []byte, model string) ([]byte, error) {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(in, &src); err != nil {
		return nil, fmt.Errorf("请求体解析失败: %w", err)
	}

	out := map[string]any{"model": model}

	var systemParts []string
	var msgs []map[string]any

	var list []map[string]json.RawMessage
	if raw, ok := src["messages"]; ok {
		_ = json.Unmarshal(raw, &list)
	}

	for _, m := range list {
		role := jsonString(m["role"])
		switch role {
		case "system", "developer":
			if t := extractText(m["content"]); t != "" {
				systemParts = append(systemParts, t)
			}
			continue
		case "tool":
			// 工具结果 → user 消息里的 tool_result block
			msgs = append(msgs, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": jsonString(m["tool_call_id"]),
					"content":     extractText(m["content"]),
				}},
			})
			continue
		}

		content := []map[string]any{}
		if t := extractText(m["content"]); t != "" {
			content = append(content, map[string]any{"type": "text", "text": t})
		}
		// assistant 的工具调用
		if raw, ok := m["tool_calls"]; ok {
			var calls []map[string]json.RawMessage
			if json.Unmarshal(raw, &calls) == nil {
				for _, c := range calls {
					var fn map[string]json.RawMessage
					_ = json.Unmarshal(c["function"], &fn)
					var input any = map[string]any{}
					args := jsonString(fn["arguments"])
					if args != "" {
						_ = json.Unmarshal([]byte(args), &input)
					}
					content = append(content, map[string]any{
						"type":  "tool_use",
						"id":    jsonString(c["id"]),
						"name":  jsonString(fn["name"]),
						"input": input,
					})
				}
			}
		}
		if len(content) == 0 {
			content = append(content, map[string]any{"type": "text", "text": ""})
		}
		msgs = append(msgs, map[string]any{"role": role, "content": content})
	}

	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	out["messages"] = msgs

	// max_tokens 必填
	if raw, ok := src["max_tokens"]; ok {
		out["max_tokens"] = raw
	} else if raw, ok := src["max_completion_tokens"]; ok {
		out["max_tokens"] = raw
	} else {
		out["max_tokens"] = 4096
	}

	for _, k := range []string{"temperature", "top_p"} {
		if raw, ok := src[k]; ok {
			out[k] = raw
		}
	}
	if raw, ok := src["stop"]; ok {
		var stops []string
		if json.Unmarshal(raw, &stops) == nil {
			out["stop_sequences"] = stops
		} else {
			var single string
			if json.Unmarshal(raw, &single) == nil && single != "" {
				out["stop_sequences"] = []string{single}
			}
		}
	}
	if raw, ok := src["stream"]; ok {
		out["stream"] = raw
	}

	// 工具定义
	if raw, ok := src["tools"]; ok {
		var tools []map[string]json.RawMessage
		if json.Unmarshal(raw, &tools) == nil {
			var converted []map[string]any
			for _, t := range tools {
				var fn map[string]json.RawMessage
				_ = json.Unmarshal(t["function"], &fn)
				name := jsonString(fn["name"])
				if name == "" {
					continue
				}
				item := map[string]any{"name": name}
				if d, ok := fn["description"]; ok {
					item["description"] = d
				}
				if s, ok := fn["parameters"]; ok {
					item["input_schema"] = s
				} else {
					item["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				converted = append(converted, item)
			}
			if len(converted) > 0 {
				out["tools"] = converted
			}
		}
	}
	if raw, ok := src["tool_choice"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch s {
			case "auto":
				out["tool_choice"] = map[string]any{"type": "auto"}
			case "required":
				out["tool_choice"] = map[string]any{"type": "any"}
			case "none":
				// Anthropic 没有 none，省掉工具即可
			}
		} else {
			var tc map[string]json.RawMessage
			if json.Unmarshal(raw, &tc) == nil {
				var fn map[string]json.RawMessage
				_ = json.Unmarshal(tc["function"], &fn)
				if name := jsonString(fn["name"]); name != "" {
					out["tool_choice"] = map[string]any{"type": "tool", "name": name}
				}
			}
		}
	}

	return json.Marshal(out)
}
