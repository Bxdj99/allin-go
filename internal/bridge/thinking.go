package bridge

import (
	"encoding/json"
	"strings"
)

// thinking.go 负责把 Anthropic 的思考块映射成 OpenAI 风格的 reasoning_content。
//
// 为什么只做单向：Anthropic 的 thinking block 带 signature（防篡改签名），
// 转成纯文本 reasoning_content 后签名就丢了，再喂回上游会被拒。
// 所以只做「上游 Anthropic → 客户端 OpenAI」这个方向的输出映射，
// 让 OpenAI 客户端（Cherry Studio / Chatbox 这类）能展开思考过程；
// 反向不做，避免把已签名的内容弄坏。

// AnthropicThinking 是从 Anthropic 响应里抽出来的思考内容。
type AnthropicThinking struct {
	Text string
	// Redacted 表示这是被加密/脱敏的思考块，只能透传不能解码
	Redacted bool
}

// ExtractThinking 从 Anthropic message 响应里抽出所有 thinking 块。
//
// 会同时返回去掉思考块之后的 content（用于正常正文），
// 以及思考文本（用于塞进 reasoning_content）。
func ExtractThinking(content []map[string]any) (text string, hasThinking bool) {
	var sb strings.Builder
	for _, block := range content {
		typ, _ := block["type"].(string)
		switch typ {
		case "thinking":
			if t, ok := block["thinking"].(string); ok && t != "" {
				sb.WriteString(t)
			}
			hasThinking = true
		case "redacted_thinking":
			// 加密块无法解码，跳过（不往正文里塞乱码）
			hasThinking = true
		}
	}
	return sb.String(), hasThinking
}

// WithReasoningContent 给 OpenAI 形态的 message 补上 reasoning_content 字段。
func WithReasoningContent(msg map[string]any, text string) map[string]any {
	if text == "" {
		return msg
	}
	msg["reasoning_content"] = text
	return msg
}

// AnthropicSSEToOpenAIChunk 把 Anthropic 的一条 SSE 事件转成 OpenAI chunk 需要的片段。
//
// 返回 (contentDelta, reasoningDelta, done, valid)：
//   - contentDelta   正文增量
//   - reasoningDelta 思考增量（放进 delta.reasoning_content）
//   - done           是否终止
//   - valid          这条事件是否值得转发
func AnthropicSSEToOpenAIChunk(event map[string]json.RawMessage) (contentDelta, reasoningDelta string, done, valid bool) {
	var typ string
	_ = json.Unmarshal(event["type"], &typ)

	switch typ {
	case "content_block_delta":
		var delta map[string]json.RawMessage
		if json.Unmarshal(event["delta"], &delta) != nil {
			return "", "", false, false
		}
		var dt string
		_ = json.Unmarshal(delta["type"], &dt)
		switch dt {
		case "text_delta":
			var t string
			_ = json.Unmarshal(delta["text"], &t)
			return t, "", false, t != ""
		case "thinking_delta":
			var t string
			_ = json.Unmarshal(delta["thinking"], &t)
			return "", t, false, t != ""
		}
		return "", "", false, false

	case "message_delta":
		// 带 stop_reason 与 usage，算一次有效的收尾信号
		return "", "", false, true

	case "message_stop":
		return "", "", true, false

	case "content_block_start", "content_block_stop", "message_start", "ping":
		return "", "", false, false
	}
	return "", "", false, false
}
