package gateway

import (
	"bytes"
	"encoding/json"
	"math"
)

// 缓存用量的解析。
//
// 为什么要专门处理：上游各家返回的字段名完全不一样，
// 而且「缺失」和「明确为 0」是两件事 ——
//   缺失 = 不知道，命中率分母不该算它，也不该显示 0%
//   0    = 确实没命中，这是个有意义的结果
// 把这两种情况混为一谈，面板上的命中率就会撒谎。
//
// 字段清单对齐上游 CacheUsage.java:22-40。

// cacheCount 从 JSON 里取一个非负整数字段。
// 不是数字、负数、小数、超出安全整数范围的，一律视为「不知道」。
func cacheCount(obj map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := obj[key]
	if !ok {
		return 0, false
	}
	// 必须是 JSON 数字字面量。json.Number 会接受带引号的 "7808"，
	// 但那种上游返回来的是字符串形态，语义上不算可信计数。
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' || string(trimmed) == "null" {
		return 0, false
	}
	var num json.Number
	if json.Unmarshal(raw, &num) != nil {
		return 0, false
	}
	f, err := num.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f != math.Trunc(f) {
		return 0, false
	}
	if f > 9007199254740991 {
		return 0, false
	}
	return int64(f), true
}

// cacheSub 取一个嵌套对象，取不到返回 nil。
func cacheSub(obj map[string]json.RawMessage, key string) map[string]json.RawMessage {
	raw, ok := obj[key]
	if !ok {
		return nil
	}
	var sub map[string]json.RawMessage
	if json.Unmarshal(raw, &sub) != nil {
		return nil
	}
	return sub
}

// cachePrefer 按顺序取第一个「有意义」的值：
// 有正数就用正数（标准字段优先），全是 0 才用第一个 0。
//
// 语义与上游 prefer() 一致：**一个非零的标准字段压过所有别名**，
// 只有标准字段明确是 0 时才允许回退去读别名。
func cachePrefer(vals ...struct {
	v  int64
	ok bool
}) (int64, bool) {
	var zero int64
	hasZero := false
	for _, it := range vals {
		if !it.ok {
			continue
		}
		if it.v > 0 {
			return it.v, true
		}
		if !hasZero {
			zero = it.v
			hasZero = true
		}
	}
	if hasZero {
		return zero, true
	}
	return 0, false
}

func cc(obj map[string]json.RawMessage, key string) struct {
	v  int64
	ok bool
} {
	v, ok := cacheCount(obj, key)
	return struct {
		v  int64
		ok bool
	}{v, ok}
}

// cacheReadFrom 解析「读缓存」的 token 数。
//
// 优先级：
//
//	prompt_tokens_details.cached_tokens
//	input_tokens_details.cached_tokens
//	prompt_cache_hit_tokens
//	cached_tokens
//	num_cached_tokens
//	cache_read_input_tokens / cacheReadInputTokens
//	cachedContentTokenCount
func cacheReadFrom(u map[string]json.RawMessage) (int64, bool) {
	if u == nil {
		return 0, false
	}
	pd := cacheSub(u, "prompt_tokens_details")
	id := cacheSub(u, "input_tokens_details")
	return cachePrefer(
		cc(pd, "cached_tokens"),
		cc(id, "cached_tokens"),
		cc(u, "prompt_cache_hit_tokens"),
		cc(u, "cached_tokens"),
		cc(u, "num_cached_tokens"),
		cc(u, "cache_read_input_tokens"),
		cc(u, "cacheReadInputTokens"),
		cc(u, "cachedContentTokenCount"),
	)
}

// cacheWriteFrom 解析「写缓存」的 token 数。
//
//	prompt_tokens_details.cache_write_tokens
//	input_tokens_details.cache_write_tokens
//	cache_creation_input_tokens
//	cacheCreationInputTokens
func cacheWriteFrom(u map[string]json.RawMessage) (int64, bool) {
	if u == nil {
		return 0, false
	}
	pd := cacheSub(u, "prompt_tokens_details")
	id := cacheSub(u, "input_tokens_details")
	return cachePrefer(
		cc(pd, "cache_write_tokens"),
		cc(id, "cache_write_tokens"),
		cc(u, "cache_creation_input_tokens"),
		cc(u, "cacheCreationInputTokens"),
	)
}

// cacheHitRate 计算命中率。
//
// 分母用「完整输入量」。注意各家口径不同：
//   - OpenAI/WorkBuddy：prompt_tokens 已含缓存部分，直接拿它当分母
//   - Anthropic：input_tokens 不含缓存，得把 read+write 加回去
//
// 返回 (rate, known)。known=false 表示数据不足以计算，界面应显示「—」而不是 0%。
func cacheHitRate(cacheRead, cacheWrite int64, hasRead, hasWrite bool, prompt, cachedTotal int) (float64, bool) {
	if !hasRead && !hasWrite {
		return 0, false
	}
	denom := prompt
	if cachedTotal > prompt {
		// 缓存量比 prompt 还大，说明 prompt 是不含缓存的 Anthropic 口径
		denom = cachedTotal
	}
	if denom <= 0 {
		return 0, false
	}
	read := cacheRead
	if !hasRead {
		read = 0
	}
	rate := float64(read) / float64(denom)
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	return rate, true
}

// attachCacheUsage 从响应体里抽缓存字段，填进 usageTokens。
//
// 用原始 JSON 二次解析而不是改结构体：上游 usage 的字段形态差异太大
// （有的嵌在 prompt_tokens_details，有的直接挂顶层，还有驼峰别名），
// 用 map 处理比堆一堆 struct tag 清楚得多。
//
// 取不到就保持「不知道」，不补 0。
func attachCacheUsage(u *usageTokens, raw []byte) {
	if u == nil || len(raw) == 0 {
		return
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return
	}
	u0 := cacheUsageNode(root)
	if u0 == nil {
		return
	}
	if v, ok := cacheReadFrom(u0); ok {
		u.CacheRead = v
		u.hasCacheRead = true
	}
	if v, ok := cacheWriteFrom(u0); ok {
		u.CacheWrite = v
		u.hasCacheWrite = true
	}
}

// cacheUsageNode 找到 usage 对象本体。
// 兼容三种形态：顶层 usage、message.usage（Anthropic）、response.usage。
func cacheUsageNode(root map[string]json.RawMessage) map[string]json.RawMessage {
	if u := cacheSub(root, "usage"); u != nil {
		return u
	}
	for _, wrapper := range []string{"message", "response"} {
		if w := cacheSub(root, wrapper); w != nil {
			if u := cacheSub(w, "usage"); u != nil {
				return u
			}
		}
	}
	return nil
}

// CacheHitRate 返回本次请求的缓存命中率。
//
// 返回 ok=false 表示数据不足（上游没报缓存字段），
// 调用方应当显示「—」而不是 0%。
func (u usageTokens) CacheHitRate() (float64, bool) {
	cachedTotal := u.CacheRead + u.CacheWrite
	return cacheHitRate(u.CacheRead, u.CacheWrite, u.hasCacheRead, u.hasCacheWrite, u.In, int(cachedTotal))
}

// HasCacheInfo 表示上游是否报过缓存字段。
func (u usageTokens) HasCacheInfo() bool { return u.hasCacheRead || u.hasCacheWrite }
