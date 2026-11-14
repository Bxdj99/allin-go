// Package workbuddy 处理腾讯 WorkBuddy（copilot.tencent.com）的请求适配。
//
// 为什么不能直接透传：WorkBuddy 的网关对请求体有额外约束，
// 客户端随便发的字段会被拒或静默忽略。All-in 里这部分写了一千多行，
// 这里实现最影响「能不能调通」的三条：
//
//  1. 工具名只接受 [A-Za-z0-9_-]，中文/点号/空格一律要改，且改名后
//     历史消息里的 tool_calls 与 tool 消息必须同步映射，否则上下文对不上。
//  2. tool_choice 的语义与 OpenAI 不同：none 必须连 tools 一起删；
//     function 形态要拍平成裸名字。
//  3. 支持推理的模型建议补 reasoning_effort，否则上游可能走默认档位
//     甚至直接拒绝（max_tokens 很小时不加，避免冲突）。
package workbuddy

import (
	"encoding/json"
	"regexp"
	"strings"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var invalidChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Prepare 对请求体做 WorkBuddy 兼容处理，返回改写后的 body。
//
// effort 为空表示不主动补 reasoning_effort（由 defaultEffort 参数决定）。
func Prepare(body []byte, defaultEffort string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}

	renames := map[string]string{} // 客户端原名 → 上游安全名

	fitTools(m, renames)
	normalizeToolChoice(m, renames)
	renameHistory(m, renames)
	if defaultEffort != "" {
		applyEffort(m, defaultEffort)
	}

	return json.Marshal(m)
}

// fitTools 把工具名里的非法字符替换掉，并记录映射。
func fitTools(body map[string]json.RawMessage, renames map[string]string) {
	raw, ok := body["tools"]
	if !ok || len(raw) == 0 {
		return
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return
	}

	// 先收集已占用的合法名字
	taken := map[string]bool{}
	for _, t := range tools {
		var tool struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(t, &tool) == nil && validName.MatchString(tool.Function.Name) {
			taken[tool.Function.Name] = true
		}
	}

	for i, t := range tools {
		var tool map[string]json.RawMessage
		if json.Unmarshal(t, &tool) != nil {
			continue
		}
		fnRaw, ok := tool["function"]
		if !ok {
			continue
		}
		var fn map[string]json.RawMessage
		if json.Unmarshal(fnRaw, &fn) != nil {
			continue
		}
		var name string
		if json.Unmarshal(fn["name"], &name) != nil || name == "" || validName.MatchString(name) {
			// 参数 schema 即使名字合法也可能语法不兼容，统一修一遍
			fixSchema(fn)
			buf, _ := json.Marshal(fn)
			tool["function"] = buf
			buf, _ = json.Marshal(tool)
			tools[i] = buf
			continue
		}

		safe := safeName(name, taken)
		renames[name] = safe
		fn["name"], _ = json.Marshal(safe)
		fixSchema(fn)
		buf, _ := json.Marshal(fn)
		tool["function"] = buf
		buf, _ = json.Marshal(tool)
		tools[i] = buf
	}

	buf, _ := json.Marshal(tools)
	body["tools"] = buf
}

// fixSchema 保证 parameters 是一个合法的 object schema。
func fixSchema(fn map[string]json.RawMessage) {
	raw, ok := fn["parameters"]
	if !ok || len(raw) == 0 {
		fn["parameters"] = json.RawMessage(`{"type":"object","properties":{}}`)
		return
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		fn["parameters"] = json.RawMessage(`{"type":"object","properties":{}}`)
		return
	}
	// 顶层必须是 object 且带 properties
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if _, ok := schema["properties"]; !ok {
		schema["properties"] = map[string]any{}
	}
	buf, _ := json.Marshal(schema)
	fn["parameters"] = buf
}

func safeName(name string, taken map[string]bool) string {
	base := invalidChars.ReplaceAllString(name, "_")
	if base == "" {
		base = "tool"
	}
	out := base
	for i := 2; taken[out]; i++ {
		out = base + "_" + itoa(i)
	}
	taken[out] = true
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// normalizeToolChoice 把各种形态的 tool_choice 拍成 WorkBuddy 认的形态。
func normalizeToolChoice(body map[string]json.RawMessage, renames map[string]string) {
	raw, ok := body["tool_choice"]
	if !ok {
		return
	}

	// 字符串形态
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "none":
			delete(body, "tool_choice")
			delete(body, "tools")
			delete(body, "functions")
		default:
			if mapped, ok := renames[s]; ok {
				buf, _ := json.Marshal(mapped)
				body["tool_choice"] = buf
			} else {
				delete(body, "tool_choice") // 未知字符串上游不认，去掉更安全
			}
		}
		return
	}

	// 对象形态
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		delete(body, "tool_choice")
		return
	}
	var typ string
	_ = json.Unmarshal(obj["type"], &typ)
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "none":
		delete(body, "tool_choice")
		delete(body, "tools")
		delete(body, "functions")
	case "auto", "required":
		buf, _ := json.Marshal(strings.ToLower(strings.TrimSpace(typ)))
		body["tool_choice"] = buf
	case "function":
		var fn struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(obj["function"], &fn)
		name := strings.TrimSpace(fn.Name)
		if mapped, ok := renames[name]; ok {
			name = mapped
		}
		if name == "" {
			body["tool_choice"] = json.RawMessage(`"auto"`)
		} else {
			buf, _ := json.Marshal(name)
			body["tool_choice"] = buf
		}
	default:
		delete(body, "tool_choice")
	}
}

// renameHistory 把历史消息里的工具调用名同步成安全名。
func renameHistory(body map[string]json.RawMessage, renames map[string]string) {
	if len(renames) == 0 {
		return
	}
	raw, ok := body["messages"]
	if !ok {
		return
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return
	}
	changed := false
	for _, m := range msgs {
		callsRaw, ok := m["tool_calls"]
		if !ok {
			continue
		}
		var calls []map[string]json.RawMessage
		if json.Unmarshal(callsRaw, &calls) != nil {
			continue
		}
		for _, c := range calls {
			fnRaw, ok := c["function"]
			if !ok {
				continue
			}
			var fn map[string]json.RawMessage
			if json.Unmarshal(fnRaw, &fn) != nil {
				continue
			}
			var name string
			if json.Unmarshal(fn["name"], &name) != nil {
				continue
			}
			if mapped, ok := renames[name]; ok {
				fn["name"], _ = json.Marshal(mapped)
				buf, _ := json.Marshal(fn)
				c["function"] = buf
				changed = true
			}
		}
		if changed {
			buf, _ := json.Marshal(calls)
			m["tool_calls"] = buf
		}
	}
	if changed {
		buf, _ := json.Marshal(msgs)
		body["messages"] = buf
	}
}

// applyEffort 在请求没指定档位时补一个默认 reasoning_effort。
func applyEffort(body map[string]json.RawMessage, effort string) {
	if _, ok := body["reasoning_effort"]; ok {
		return
	}
	if raw, ok := body["reasoning"]; ok && len(raw) > 0 {
		var r map[string]json.RawMessage
		if json.Unmarshal(raw, &r) == nil {
			if _, has := r["effort"]; has {
				return
			}
		}
	}
	// max_tokens 很小时不补，避免与推理档位冲突
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if raw, ok := body[k]; ok {
			var n float64
			if json.Unmarshal(raw, &n) == nil && n > 0 && n <= 1024 {
				return
			}
		}
	}
	buf, _ := json.Marshal(effort)
	body["reasoning_effort"] = buf
}
