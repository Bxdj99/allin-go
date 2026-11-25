package workbuddy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// prompt_cache_key 注入 —— 这是最直接的省钱手段。
//
// 逆向实测（WorkBuddy2API 项目 buddy-adapter.ts:706-714）：
// 同一段 8k token 前缀发两次
//
//	不带 prompt_cache_key → prompt_cache_hit_tokens=0      credit≈0.34
//	带   prompt_cache_key → prompt_cache_hit_tokens=7808   credit≈0.02
//
// 积分消耗相差约 17 倍。原因是不带 key 时上游无法把连续请求归到同一会话，
// 每次都按全量 prompt 计费。
//
// 我们之前完全没注入这个字段，这就是「同样用量，别的网关更省」的直接原因。

// InjectPromptCacheKey 给请求体加上 prompt_cache_key。
//
// 优先级：
//  1. 客户端已带 prompt_cache_key → 原样保留，不覆盖
//  2. body 里的 conversation_id / conversationId 作为会话源
//  3. 都没有 → 用传入的 sessionKey（网关按对话内容算出的指纹）
//
// 账号隔离是硬要求：key 里编入账号 uid 前缀，避免命中别的账号的前缀缓存。
// 跨账号复用同一 key 会读到对方对话，既是费用问题也是安全问题。
func InjectPromptCacheKey(body []byte, uid, sessionKey string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body // 坏 body 不动，避免二次错误化
	}

	// 1) 客户端自带的，尊重它
	if v, ok := obj["prompt_cache_key"].(string); ok && strings.TrimSpace(v) != "" {
		return body
	}

	// 2) 客户端自带的会话标识优先于网关推断。
	//
	// 顺序对齐上游 QoderProtocol.java:39：
	//   agent 明说的会话 → 客户端 prompt_cache_key → 客户端 user → 网关推断
	//
	// 为什么必须认这么多字段：agent 类客户端每轮带全量历史，
	// 一旦 key 每轮都变，前缀缓存全部失效，按全量 prompt 计费 ——
	// 这正是「输入 6.7 亿 token、输出只有 67 万」的来源。
	// 让 key 稳定比让 key 唯一重要得多。
	conv := ""
	for _, k := range []string{
		"conversation_id", "conversationId", "conversation",
		"session_id", "sessionId", "chat_id", "chatId",
		"thread_id", "threadId", "user",
	} {
		if v := strField(obj, k); v != "" {
			conv = v
			break
		}
	}
	if conv == "" {
		conv = sessionKey
	}

	obj["prompt_cache_key"] = buildCacheKey(uid, conv)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// buildCacheKey 生成 `<uid8>-<convHex>` 形式的稳定 key。
//
// uid8 提供账号隔离；convHex 由 uid+会话源哈希而来，保证：
//   - 同账号同会话 → 稳定（能命中缓存）
//   - 不同会话 → 不同（不会错误复用）
//   - 跨账号 → 绝不碰撞
func buildCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	return "allin-" + uid8 + "-" + hex.EncodeToString(sum[:16])
}

func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
