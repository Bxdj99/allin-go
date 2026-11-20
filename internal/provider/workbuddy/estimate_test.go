package workbuddy

import "testing"

// 中文必须按 1 字符 1 token 计，不能被低估
func TestEstimateCJK(t *testing.T) {
	zh := "这是一段中文测试文本" // 10 字
	if got := estimateTextTokens(zh); got != 10 {
		t.Fatalf("10 个中文应算 10 token，实际 %d", got)
	}
}

// 英文按 4 字符 1 token
func TestEstimateASCII(t *testing.T) {
	en := "abcdefgh" // 8 字符 → 2 token
	if got := estimateTextTokens(en); got != 2 {
		t.Fatalf("8 个英文字符应算 2 token，实际 %d", got)
	}
}

// 混排
func TestEstimateMixed(t *testing.T) {
	s := "中文abcd" // 2 中文 + 4 英文字符 = 2 + 1 = 3
	if got := estimateTextTokens(s); got != 3 {
		t.Fatalf("混排应算 3 token，实际 %d", got)
	}
}

func TestEstimateEmpty(t *testing.T) {
	if got := estimateTextTokens(""); got != 0 {
		t.Fatalf("空串应为 0，实际 %d", got)
	}
}

// 旧算法 char/2 会把中文低估一半，这里验证新算法确实更高
func TestEstimateNotUnderestimateChinese(t *testing.T) {
	zh := "上下文上下文上下文上下文" // 12 字
	old := len(zh) / 2            // 旧算法：字符数/2（UTF-8 下 len 是字节数，更离谱）
	n := estimateTextTokens(zh)
	if n < 12 {
		t.Fatalf("中文估算不应低于字符数，实际 %d", n)
	}
	t.Logf("12 个中文：新算法 %d token，旧 char/2 按字节算约 %d", n, old/3)
}
