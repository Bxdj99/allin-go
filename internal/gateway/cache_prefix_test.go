package gateway

import "testing"

// 缓存前缀保护：改写请求体会打乱前缀，让原本可命中的缓存全部按全价计费。
// 只有当「省下的 token」明显大于「丢失的缓存折扣」时才值得改写。

func TestRewriteWorthwhileLargeSaving(t *testing.T) {
	// 省 50%，远超 30% 阈值
	ok, _ := cachePrefixRewriteWorthwhile(200000, 100000, true)
	if !ok {
		t.Error("省一半应当值得改写")
	}
}

func TestRewriteNotWorthwhileSmallSaving(t *testing.T) {
	// 只省 10%，但会让 20 万 token 的前缀全部失效
	ok, why := cachePrefixRewriteWorthwhile(200000, 180000, true)
	if ok {
		t.Errorf("只省 10%% 不该改写，理由: %s", why)
	}
}

// 本来就没有缓存可破坏时，能省就是赚。
func TestRewriteWorthwhileWhenNoCache(t *testing.T) {
	ok, _ := cachePrefixRewriteWorthwhile(200000, 190000, false)
	if !ok {
		t.Error("没有缓存可破坏时，小节省也值得")
	}
}

// 没省到东西就不改写。
func TestRewriteRejectsNoGain(t *testing.T) {
	if ok, _ := cachePrefixRewriteWorthwhile(100000, 100000, true); ok {
		t.Error("没有减少不该改写")
	}
	if ok, _ := cachePrefixRewriteWorthwhile(100000, 120000, true); ok {
		t.Error("变大更不该改写")
	}
}

// 边界：恰好 30%。
func TestRewriteBoundary(t *testing.T) {
	// 30% 整
	if ok, _ := cachePrefixRewriteWorthwhile(100000, 70000, true); !ok {
		t.Error("省 30% 应当值得")
	}
	// 29%
	if ok, _ := cachePrefixRewriteWorthwhile(100000, 71000, true); ok {
		t.Error("省 29% 不该值得")
	}
}

// 工具占比估算：tools 在最前面，是缓存命中价值的核心来源。
func TestToolsTokenShare(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"a","description":"` +
		"这是一段比较长的工具描述用于测试占比" + `"}}],"messages":[{"role":"user","content":"hi"}]}`)
	toolsTok, share := toolsTokenShare(body)
	if toolsTok <= 0 {
		t.Fatal("应能算出 tools 的 token 数")
	}
	if share <= 0 || share > 1 {
		t.Errorf("占比应在 (0,1]，得到 %v", share)
	}
}

func TestToolsTokenShareNoTools(t *testing.T) {
	if tok, share := toolsTokenShare([]byte(`{"messages":[]}`)); tok != 0 || share != 0 {
		t.Errorf("没有 tools 时应返回 0/0，得到 %d/%v", tok, share)
	}
}

// EstimateBodyTokens 按 rune 计中文，不能被 UTF-8 多字节骗到。
func TestEstimateBodyTokensCJK(t *testing.T) {
	// 10 个中文字符应约等于 10 token
	got := EstimateBodyTokens([]byte("你好世界你好世界你好世界"))
	if got < 8 || got > 14 {
		t.Errorf("12 个中文估算 token = %d, 期望接近 12", got)
	}
}

func TestEstimateBodyTokensEmpty(t *testing.T) {
	if got := EstimateBodyTokens(nil); got != 0 {
		t.Errorf("空 body 应为 0，得到 %d", got)
	}
}
