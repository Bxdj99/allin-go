package gateway

import (
	"strings"
	"testing"
)

// 三种策略都必须有建议，漏掉一种面板上就会空一块。
func TestStrategyAdvicesCoverAll(t *testing.T) {
	got := strategyAdvices()
	if len(got) != 3 {
		t.Fatalf("应有 3 条策略建议，实际 %d", len(got))
	}
	for _, key := range []string{"order", "round-robin", "manual"} {
		a := strategyAdviceOf(key)
		if a == nil {
			t.Fatalf("策略 %s 没有建议", key)
		}
		if a.Token == "" || a.Detail == "" || a.BestFor == "" {
			t.Errorf("策略 %s 的建议字段不全: %+v", key, a)
		}
	}
}

// 结论必须站得住：顺序主备最省、轮询最费。
// 反了就等于在误导用户烧钱。
func TestStrategyAdviceRanking(t *testing.T) {
	order := strategyAdviceOf("order")
	rr := strategyAdviceOf("round-robin")
	if order == nil || rr == nil {
		t.Fatal("缺少建议")
	}
	if order.Level != "low" {
		t.Errorf("顺序主备应是 low，实际 %s", order.Level)
	}
	if rr.Level != "high" {
		t.Errorf("轮询均摊应是 high，实际 %s", rr.Level)
	}
}

// 空策略要落到 order，否则老配置（没写 strategy）在面板上无处显示建议。
func TestNormStrategy(t *testing.T) {
	cases := map[string]string{
		"":            "order",
		"order":       "order",
		"round-robin": "round-robin",
		"manual":      "manual",
		"手工乱填":        "order",
		"ROUND_ROBIN": "order",
	}
	for in, want := range cases {
		if got := normStrategy(in); got != want {
			t.Errorf("normStrategy(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// 同供应商的目标要明确告诉用户「换号代价小」，
// 跨供应商要警告「每次换都重算」—— 这两句结论相反，判错就是误导。
func TestGroupTargetTokenNoteSameProvider(t *testing.T) {
	cfg := groupTestConfig("workbuddy/glm-5.3", "workbuddy/deepseek-v4.1-flash")
	notes := groupTargetTokenNote(cfg, cfg.Groups[0])
	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "一个供应商") {
		t.Fatalf("同供应商应有对应提示，实际: %s", joined)
	}
	// 关键：即便同供应商，换目标仍然是换账号，缓存照样失效。
	// 之前这里断言「不该说重建缓存」是错的 —— 用户的纠正是对的。
	if !strings.Contains(joined, "缓存同样会失效") {
		t.Errorf("同供应商也必须说明换号会失效缓存: %s", joined)
	}
}

func TestGroupTargetTokenNoteCrossProvider(t *testing.T) {
	cfg := groupTestConfig("workbuddy/glm-5.3", "trae-cn/hy3")
	notes := groupTargetTokenNote(cfg, cfg.Groups[0])
	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "缓存必然重建") {
		t.Fatalf("跨供应商应警告缓存必然重建，实际: %s", joined)
	}
}

// 死链要单独提醒，否则用户会奇怪「为什么第一下总是慢/失败」。
func TestGroupTargetTokenNoteDeadTarget(t *testing.T) {
	cfg := groupTestConfig("workbuddy/not-exist", "workbuddy/glm-5.3")
	notes := groupTargetTokenNote(cfg, cfg.Groups[0])
	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "失效") {
		t.Fatalf("应提示有失效目标，实际: %s", joined)
	}
}

// 没有任何目标时不能 panic，也不该硬编一段假建议。
func TestGroupTargetTokenNoteEmpty(t *testing.T) {
	cfg := groupTestConfig()
	cfg.Groups[0].Targets = nil
	if notes := groupTargetTokenNote(cfg, cfg.Groups[0]); len(notes) != 0 {
		t.Fatalf("空组不该有提示，实际 %v", notes)
	}
}
