package workbuddy

import (
	"encoding/json"
	"regexp"
)

// thinking 签名恢复。
//
// 背景：这个上游对 assistant 历史里的 thinking 块有签名校验。
// 签名不是它自己签发的时候，会返回 11159 或 11133（thinking_signature_invalid），
// 把整个请求拒掉。而**同一段历史，只要不带 thinking 块就能通过**。
//
// 典型触发：客户端（尤其 agent 类）带来的历史里夹着别的渠道签发的 thinking 块，
// 原样转发就会被拒。这也是「App 版能用、网关版被拦」的直接原因——
// App 里有这套恢复逻辑，网关之前没有。
//
// 处理原则：
//   - 只对状态码 >= 400 的确定性拒绝生效
//   - 只在错误信息明确指向签名问题时生效
//   - 只删 thinking 块；visible text / tool_calls / tool 结果 / redacted_thinking 一律保留
//   - 本来就没有 thinking 块时返回 nil，调用方据此不重发
var signatureRefusal = regexp.MustCompile(`(?i)thinking_signature_invalid|invalid\s+` + "`?" +
	`signature` + "`?" + `\s+in\s+` + "`?" + `thinking|block is bound to a different conversation|` +
	`["']internal_code["']\s*:\s*["']?11159`)

// IsSignatureRefusal 判断一次上游拒绝是不是 thinking 签名问题。
func IsSignatureRefusal(status int, body string) bool {
	// 只认 4xx：5xx 是上游故障，去掉 thinking 重发也不会好，反而多打一次请求。
	if status < 400 || status >= 500 {
		return false
	}
	if body == "" || len(body) > 1<<20 {
		return false
	}
	return signatureRefusal.MatchString(body)
}

// WithoutThinkingBlocks 返回删掉所有 thinking 块后的请求体。
//
// 返回 nil 表示「没有可删的」——调用方据此判断不该重发，
// 避免把一模一样的请求再打一遍。
func WithoutThinkingBlocks(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	rawMsgs, ok := root["messages"]
	if !ok {
		return nil
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(rawMsgs, &msgs) != nil {
		return nil
	}

	dropped := 0
	for _, m := range msgs {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role != "assistant" {
			continue
		}
		rawContent, ok := m["content"]
		if !ok {
			continue
		}
		// 只有数组形态才可能含 thinking 块
		var blocks []json.RawMessage
		if json.Unmarshal(rawContent, &blocks) != nil {
			continue
		}
		kept := make([]json.RawMessage, 0, len(blocks))
		removed := false
		for _, b := range blocks {
			var t struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(b, &t)
			if t.Type == "thinking" {
				removed = true
				dropped++
				continue
			}
			kept = append(kept, b)
		}
		if !removed {
			continue
		}
		// 删空了补一个空 text 块——上游不接受 content 为空的 assistant 消息
		if len(kept) == 0 {
			empty, _ := json.Marshal(map[string]any{"type": "text", "text": ""})
			kept = append(kept, empty)
		}
		buf, err := json.Marshal(kept)
		if err != nil {
			continue
		}
		m["content"] = buf
	}

	if dropped == 0 {
		return nil
	}
	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return nil
	}
	root["messages"] = newMsgs
	out, err := json.Marshal(root)
	if err != nil {
		return nil
	}
	return out
}

// StripThinkingBlocks 无条件删掉 thinking 块（用于「不带思考历史」的选项）。
// 返回原 body 表示没有变化。
func StripThinkingBlocks(body []byte) []byte {
	if out := WithoutThinkingBlocks(body); out != nil {
		return out
	}
	return body
}
