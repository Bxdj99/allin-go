package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

)

// 会话指纹提取。
//
// 目的：让「同一段对话的连续请求」落到同一个账号上（会话绑定）。
// 不要求客户端传会话 id，纯从请求内容推断。
//
// 取法：第一条 user 消息的内容。理由：
//   - 它在整段对话里始终不变，而后续消息每轮都在追加
//   - 用它算出的 key 在同一会话里稳定，换会话（新开）立刻变化
//
// 兜底：没有 user 消息时取前两条消息拼起来；都没有则返回空
// （返回空表示不做绑定，退化成普通选号）。

// sessionFingerprint 从请求体算出会话指纹。
func sessionFingerprint(body []byte) string {
	var m struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if len(m.Messages) == 0 {
		return ""
	}

	for _, raw := range m.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if msg.Role != "user" {
			continue
		}
		text := contentText(msg.Content)
		if text == "" {
			continue
		}
		return hashText(text)
	}

	// 没有 user 消息：用前两条消息的整体做指纹
	n := len(m.Messages)
	if n > 2 {
		n = 2
	}
	var joined []byte
	for i := 0; i < n; i++ {
		joined = append(joined, m.Messages[i]...)
	}
	if len(joined) == 0 {
		return ""
	}
	return hashText(string(joined))
}

// contentText 把 content 字段统一成字符串。
// OpenAI 里它可能是字符串，也可能是 [{"type":"text","text":"..."}] 数组。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var out string
		for _, p := range parts {
			if p.Type == "text" || p.Type == "" {
				out += p.Text
			}
		}
		return out
	}
	return ""
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
