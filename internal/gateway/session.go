package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
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
//
// ⚠️ 这个指纹同时被用作 prompt_cache_key 的会话段（见 cache_key.go），
// 所以「稳定」比「唯一」重要得多：同一条对话的每一轮都必须算出同一个值，
// 否则上游前缀缓存全部失效，每轮按全量 prompt 计费。
// 线上实测到的后果是输入 6.7 亿 token / 输出 67 万（1005:1）。
//
// 取法优先级：
//  1. 客户端自带的会话标识（最稳，agent 类客户端通常都有）
//  2. 第一条 **非 system** 的 user 消息
//  3. 前两条消息整体
//
// 为什么是「非 system 的 user」而不是「第一条 user」：
// agent 客户端常把 system reminder / 环境注入塞进 user 角色，
// 这类内容会随目录树、时间、工具状态变化。用它当指纹，
// 只要用户 cd 一下或者加个文件，缓存 key 就整个换掉。
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

	// 1) 客户端会话标识优先
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) == nil {
		for _, k := range []string{
			"conversation_id", "conversationId", "conversation",
			"session_id", "sessionId", "chat_id", "chatId",
			"thread_id", "threadId", "user",
		} {
			var v string
			if raw, ok := env[k]; ok && json.Unmarshal(raw, &v) == nil {
				if v = strings.TrimSpace(v); v != "" {
					return hashText(v)
				}
			}
		}
	}

	// 2) 第一条有实质内容的 user 消息
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
		// 纯注入类的首条消息不适合当指纹：它随环境变动。
		if looksLikeInjectedContext(text) {
			continue
		}
		return hashText(text)
	}

	// 回退：用第一条 user 消息（即使是注入内容），总比空指纹强
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
		if text := contentText(msg.Content); text != "" {
			return hashText(text)
		}
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

// looksLikeInjectedContext 判断一条 user 消息是不是客户端塞进来的环境注入，
// 而不是用户真正打的字。
//
// 这类内容随目录、时间、工具状态变化，拿它当会话指纹会让缓存 key 每轮都变。
// 判据取自 agent 客户端常见的注入形态，宁可漏判也别误判 ——
// 漏判只是退化到旧行为，误判会把真实对话当成注入而跳过。
func looksLikeInjectedContext(text string) bool {
	const maxProbe = 4000
	if len(text) > maxProbe {
		text = text[:maxProbe]
	}
	lower := strings.ToLower(text)
	markers := []string{
		"<system-reminder>", "</system-reminder>",
		"<environment_details>", "</environment_details>",
		"# environment", "current working directory:",
		"<user_info>", "<cwd>", "<ide_",
		"you are claude code", "you are an ai assistant",
	}
	for _, mk := range markers {
		if strings.Contains(lower, mk) {
			return true
		}
	}
	return false
}
