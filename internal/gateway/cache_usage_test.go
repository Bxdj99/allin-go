package gateway

import (
	"encoding/json"
	"testing"
)

// 缓存用量解析。核心要求：
//  1. 上游各家字段名不同，都要认
//  2. 「缺失」不等于「0」—— 缺失时命中率必须显示「—」而不是 0%

func TestCacheReadFromAliases(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{
			name: "OpenAI 标准 prompt_tokens_details",
			body: `{"prompt_tokens_details":{"cached_tokens":7808}}`,
			want: 7808,
		},
		{
			name: "input_tokens_details 变体",
			body: `{"input_tokens_details":{"cached_tokens":1234}}`,
			want: 1234,
		},
		{
			name: "DeepSeek prompt_cache_hit_tokens",
			body: `{"prompt_cache_hit_tokens":5600}`,
			want: 5600,
		},
		{
			name: "顶层 cached_tokens",
			body: `{"cached_tokens":900}`,
			want: 900,
		},
		{
			name: "num_cached_tokens",
			body: `{"num_cached_tokens":77}`,
			want: 77,
		},
		{
			name: "Anthropic cache_read_input_tokens",
			body: `{"cache_read_input_tokens":2048}`,
			want: 2048,
		},
		{
			name: "Anthropic 驼峰",
			body: `{"cacheReadInputTokens":1024}`,
			want: 1024,
		},
		{
			name: "Google cachedContentTokenCount",
			body: `{"cachedContentTokenCount":512}`,
			want: 512,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var root map[string]json.RawMessage
			if err := json.Unmarshal([]byte(c.body), &root); err != nil {
				t.Fatal(err)
			}
			got, ok := cacheReadFrom(root)
			if !ok {
				t.Fatal("应能解析出来")
			}
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// 标准字段非 0 时压过别名；只有标准字段明确是 0 才回退读别名。
func TestCacheReadPreferStandard(t *testing.T) {
	var root map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"prompt_tokens_details":{"cached_tokens":500},"prompt_cache_hit_tokens":9999}`), &root)
	if got, _ := cacheReadFrom(root); got != 500 {
		t.Errorf("标准字段应压过别名，got %d, want 500", got)
	}

	var root2 map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":888}`), &root2)
	if got, _ := cacheReadFrom(root2); got != 888 {
		t.Errorf("标准字段为 0 时应回退别名，got %d, want 888", got)
	}
}

// 脏数据不能当成有效计数。
func TestCacheCountRejectsInvalid(t *testing.T) {
	bad := []string{
		`{"prompt_tokens_details":{"cached_tokens":-5}}`,
		`{"prompt_tokens_details":{"cached_tokens":1.5}}`,
		`{"prompt_tokens_details":{"cached_tokens":"7808"}}`,
		`{"prompt_tokens_details":{"cached_tokens":null}}`,
	}
	for _, b := range bad {
		var root map[string]json.RawMessage
		_ = json.Unmarshal([]byte(b), &root)
		if _, ok := cacheReadFrom(root); ok {
			t.Errorf("%s 不应被当成有效计数", b)
		}
	}
}

// 没有任何缓存字段时，不能凭空造 0。
func TestCacheAbsentIsNotZero(t *testing.T) {
	u := usageTokens{In: 1000, Out: 10}
	if u.HasCacheInfo() {
		t.Error("没有缓存字段时 HasCacheInfo 应为 false")
	}
	if _, known := u.CacheHitRate(); known {
		t.Error("没有缓存字段时命中率应未知，不能是 0%")
	}
}

// 明确上报 0 命中，是有效结论。
func TestCacheExplicitZero(t *testing.T) {
	raw := []byte(`{"usage":{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":0}}}`)
	u := usageFromJSON(raw)
	if !u.HasCacheInfo() {
		t.Fatal("明确上报 0 也应算有数据")
	}
	rate, known := u.CacheHitRate()
	if !known {
		t.Fatal("命中率应可计算")
	}
	if rate != 0 {
		t.Errorf("命中率应为 0，得到 %v", rate)
	}
}

// 正常命中场景：8k 前缀，命中 7808。
func TestCacheHitRateTypical(t *testing.T) {
	raw := []byte(`{"usage":{"prompt_tokens":8000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":7808}}}`)
	u := usageFromJSON(raw)
	rate, known := u.CacheHitRate()
	if !known {
		t.Fatal("应能算出命中率")
	}
	want := 7808.0 / 8000.0
	if rate < want-0.001 || rate > want+0.001 {
		t.Errorf("命中率 = %.4f, want %.4f", rate, want)
	}
}

// Anthropic 口径：input_tokens 不含缓存，分母要加回去。
func TestCacheHitRateAnthropic(t *testing.T) {
	u := usageTokens{In: 2000, Out: 100, CacheRead: 8000, hasCacheRead: true}
	rate, known := u.CacheHitRate()
	if !known {
		t.Fatal("应能算出命中率")
	}
	// 分母取 cachedTotal(8000) > In(2000)，所以是 8000/8000 = 1
	if rate < 0.99 {
		t.Errorf("Anthropic 口径应与缓存量同量级，得到 %.4f", rate)
	}
}

// 流式帧也要能抽到缓存字段。
func TestCacheFromStreamEvent(t *testing.T) {
	raw := []byte(`{"id":"x","usage":{"prompt_tokens":5000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":4000}}}`)
	u := usageFromEvent(raw)
	if !u.HasCacheInfo() {
		t.Fatal("流式帧应能解析缓存字段")
	}
	if u.CacheRead != 4000 {
		t.Errorf("CacheRead = %d, want 4000", u.CacheRead)
	}
}

// Anthropic 流式：message.usage 里的 cache_read_input_tokens。
func TestCacheFromAnthropicStream(t *testing.T) {
	raw := []byte(`{"type":"message_start","message":{"usage":{"input_tokens":300,"cache_read_input_tokens":6000,"cache_creation_input_tokens":200}}}`)
	u := usageFromEvent(raw)
	if u.CacheRead != 6000 {
		t.Errorf("CacheRead = %d, want 6000", u.CacheRead)
	}
	if u.CacheWrite != 200 {
		t.Errorf("CacheWrite = %d, want 200", u.CacheWrite)
	}
}

// 多帧合并时缓存量取最大（累计值语义），且 has 标记不能被 0 抹掉。
func TestCacheAddKeepsKnownFlag(t *testing.T) {
	var u usageTokens
	u.add(usageTokens{CacheRead: 5000, hasCacheRead: true})
	u.add(usageTokens{CacheRead: 0, hasCacheRead: false})
	if !u.hasCacheRead || u.CacheRead != 5000 {
		t.Errorf("后续无缓存信息的帧不应抹掉已知值，得到 %d/%v", u.CacheRead, u.hasCacheRead)
	}
	u.add(usageTokens{CacheRead: 6000, hasCacheRead: true})
	if u.CacheRead != 6000 {
		t.Errorf("累计值应取最大，得到 %d", u.CacheRead)
	}
}
