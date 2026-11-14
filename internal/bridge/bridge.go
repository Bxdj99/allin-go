// Package bridge 做 Anthropic Messages 与 OpenAI Chat Completions 之间的协议互转。
//
// 为什么要它：客户端（Claude Code、Cursor、各种 SDK）可能只会说一种协议，
// 而你手上的供应商是另一种。有了互转，任一端点都能路由到任一家上游。
//
// 覆盖范围：
//   - 请求：system 消息、多段 content、tools / tool_choice、常用采样参数
//   - 响应：非流式一次性转换；流式按事件逐个转
//   - 未识别的字段一律丢弃而不是报错（宁可少点参数也别整条请求失败）
package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ---------- 请求：Anthropic → OpenAI ----------

// AnthropicToOpenAI 把 Anthropic Messages 请求体转成 OpenAI Chat Completions 请求体。
func AnthropicToOpenAI(in []byte, model string) ([]byte, error) {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(in, &src); err != nil {
		return nil, fmt.Errorf("请求体解析失败: %w", err)
	}

	out := map[string]any{"model": model}
	var msgs []map[string]any

	// system 可能是字符串，也可能是 content block 数组
	if raw, ok := src["system"]; ok {
		if text := extractText(raw); text != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": text})
		}
	}

	if raw, ok := src["messages"]; ok {
		var list []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &list); err == nil {
			for _, m := range list {
				role := jsonString(m["role"])
				converted := map[string]any{"role": role}

				// content 可能是字符串，也可能是 block 数组
				if c, ok := m["content"]; ok {
					if s := jsonString(c); s != "" || bytes.HasPrefix(bytes.TrimSpace(c), []byte(`"`)) {
						converted["content"] = jsonString(c)
					} else {
						texts, toolCalls, toolResults := splitBlocks(c)
						switch {
						case role == "assistant" && len(toolCalls) > 0:
							converted["content"] = strings.Join(texts, "")
							converted["tool_calls"] = toolCalls
						case role == "user" && len(toolResults) > 0:
							// OpenAI 把工具结果作为独立的 role=tool 消息
							for _, tr := range toolResults {
								msgs = append(msgs, tr)
							}
							if len(texts) > 0 {
								converted["content"] = strings.Join(texts, "")
							} else {
								continue
							}
						default:
							converted["content"] = strings.Join(texts, "")
						}
					}
				}
				msgs = append(msgs, converted)
			}
		}
	}
	out["messages"] = msgs

	// 采样参数（名字基本一致）
	for _, k := range []string{"temperature", "top_p", "top_k"} {
		if raw, ok := src[k]; ok {
			out[k] = raw
		}
	}
	// max_tokens 必填语义在 OpenAI 里是可选
	if raw, ok := src["max_tokens"]; ok {
		out["max_tokens"] = raw
	}
	if raw, ok := src["stop_sequences"]; ok {
		var stops []string
		if json.Unmarshal(raw, &stops) == nil && len(stops) > 0 {
			out["stop"] = stops
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
				name := jsonString(t["name"])
				if name == "" {
					continue
				}
				fn := map[string]any{"name": name}
				if d, ok := t["description"]; ok {
					fn["description"] = d
				}
				if s, ok := t["input_schema"]; ok {
					fn["parameters"] = s
				}
				converted = append(converted, map[string]any{"type": "function", "function": fn})
			}
			if len(converted) > 0 {
				out["tools"] = converted
			}
		}
	}
	if raw, ok := src["tool_choice"]; ok {
		var tc map[string]json.RawMessage
		if json.Unmarshal(raw, &tc) == nil {
			switch jsonString(tc["type"]) {
			case "auto":
				out["tool_choice"] = "auto"
			case "any":
				out["tool_choice"] = "required"
			case "tool":
				out["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": jsonString(tc["name"])},
				}
			}
		}
	}

	return json.Marshal(out)
}

// splitBlocks 把 Anthropic 的 content block 数组拆成文本 / 工具调用 / 工具结果。
func splitBlocks(raw json.RawMessage) (texts []string, toolCalls []map[string]any, toolResults []map[string]any) {
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, nil, nil
	}
	for _, b := range blocks {
		switch jsonString(b["type"]) {
		case "text":
			texts = append(texts, jsonString(b["text"]))
		case "tool_use":
			var input any = map[string]any{}
			if r, ok := b["input"]; ok {
				_ = json.Unmarshal(r, &input)
			}
			args, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]any{
				"id":   jsonString(b["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      jsonString(b["name"]),
					"arguments": string(args),
				},
			})
		case "tool_result":
			text := extractText(b["content"])
			if text == "" {
				text = "{}"
			}
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"tool_call_id": jsonString(b["tool_use_id"]),
				"content":      text,
			})
		}
	}
	return
}

// extractText 从「字符串或 block 数组」里取出纯文本。
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s := jsonString(raw); s != "" {
		return s
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if jsonString(b["type"]) == "text" {
			sb.WriteString(jsonString(b["text"]))
		}
	}
	return sb.String()
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// ---------- 响应：OpenAI → Anthropic ----------

// OpenAIToAnthropic 把 OpenAI 的 chat.completion 响应转成 Anthropic message 响应。
func OpenAIToAnthropic(in []byte, model string) ([]byte, error) {
	var src struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(in, &src); err != nil {
		return nil, fmt.Errorf("上游响应解析失败: %w", err)
	}

	content := []map[string]any{}
	stopReason := "end_turn"
	if len(src.Choices) > 0 {
		ch := src.Choices[0]
		if ch.Message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": ch.Message.Content})
		}
		for _, tc := range ch.Message.ToolCalls {
			var input any = map[string]any{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
			})
			stopReason = "tool_use"
		}
		switch ch.FinishReason {
		case "length":
			stopReason = "max_tokens"
		case "tool_calls":
			stopReason = "tool_use"
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	out := map[string]any{
		"id":          normalizeID(src.ID),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  src.Usage.PromptTokens,
			"output_tokens": src.Usage.CompletionTokens,
		},
	}
	return json.Marshal(out)
}

func normalizeID(id string) string {
	if id == "" {
		return "msg_allin"
	}
	return id
}

// ---------- 响应：Anthropic → OpenAI ----------

// AnthropicToOpenAIResponse 把 Anthropic message 响应转成 OpenAI chat.completion。
func AnthropicToOpenAIResponse(in []byte, model string) ([]byte, error) {
	var src struct {
		ID      string `json:"id"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(in, &src); err != nil {
		return nil, fmt.Errorf("上游响应解析失败: %w", err)
	}

	var text strings.Builder
	var toolCalls []map[string]any
	for _, c := range src.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			args := "{}"
			if len(c.Input) > 0 {
				args = string(c.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": args},
			})
		}
	}

	finish := "stop"
	switch src.StopReason {
	case "max_tokens":
		finish = "length"
	case "tool_use":
		finish = "tool_calls"
	}

	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	out := map[string]any{
		"id":      normalizeID(src.ID),
		"object":  "chat.completion",
		"model":   model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens":     src.Usage.InputTokens,
			"completion_tokens": src.Usage.OutputTokens,
			"total_tokens":      src.Usage.InputTokens + src.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}
