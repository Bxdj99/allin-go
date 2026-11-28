package gateway

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 造一个最小的配置：两个供应商各一个模型，能真实解析出来。
//
// apiKey 不是摆设：Resolve 要求供应商必须有可用账号（没有 accounts 时
// 退回 provider.apiKey），没它每个目标都解析失败，groupOrder 会把它们
// 全判成死的然后退回原始顺序 —— 测试就会假通过（看起来「有序」，其实没筛）。
func groupTestConfig(targets ...string) *config.Config {
	return &config.Config{
		Providers: []*config.Provider{
			{
				ID: "workbuddy", Name: "WorkBuddy", Kind: "openai", Enabled: boolPtr(true),
				APIKey:        "test-key",
				Models:        []string{"deepseek-v4.1-flash", "glm-5.3"},
				ExposedModels: []string{"deepseek-v4.1-flash", "glm-5.3"},
			},
			{
				ID: "trae-cn", Name: "Trae", Kind: "openai", Enabled: boolPtr(true),
				APIKey:        "test-key",
				Models:        []string{"hy3"},
				ExposedModels: []string{"hy3"},
			},
		},
		Groups: []*config.Group{
			{ID: "g1", Strategy: "order", Targets: targets},
		},
		Routing: config.Routing{GroupsEnabled: true},
	}
}

// 顺序主备：永远按配置顺序给，多个候选都在时第一个不动。
func TestGroupOrderOrderStrategy(t *testing.T) {
	cfg := groupTestConfig("workbuddy/glm-5.3", "workbuddy/deepseek-v4.1-flash")
	g := cfg.Groups[0]
	for i := 0; i < 3; i++ {
		got := groupOrder(cfg, g)
		if len(got) != 2 || got[0] != "workbuddy/glm-5.3" {
			t.Fatalf("第 %d 次 order 策略应固定以第一个为主，实际 %v", i, got)
		}
	}
}

// 轮询均摊：连续请求必须换目标，否则「防单号撞限流」无从谈起。
func TestGroupOrderRoundRobin(t *testing.T) {
	cfg := groupTestConfig("workbuddy/glm-5.3", "workbuddy/deepseek-v4.1-flash")
	cfg.Groups[0].Strategy = "round-robin"
	g := cfg.Groups[0]

	first := groupOrder(cfg, g)
	second := groupOrder(cfg, g)
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("不应为空")
	}
	if first[0] == second[0] {
		t.Fatalf("轮询第二次应换目标，两次都是 %q", first[0])
	}
	// 两个目标都必须在返回顺序里，才谈得上故障顺延
	if first[0] == first[1] {
		t.Fatalf("返回顺序里出现重复目标: %v", first)
	}
}

// 手动指定：只给 pick 一个，不做顺延。
func TestGroupOrderManualStrategy(t *testing.T) {
	cfg := groupTestConfig("workbuddy/glm-5.3", "workbuddy/deepseek-v4.1-flash")
	cfg.Groups[0].Strategy = "manual"
	cfg.Groups[0].Pick = "workbuddy/deepseek-v4.1-flash"
	got := groupOrder(cfg, cfg.Groups[0])
	if len(got) != 1 || got[0] != "workbuddy/deepseek-v4.1-flash" {
		t.Fatalf("manual 策略应只给 pick 指定的目标，实际 %v", got)
	}
}

// 解析不出来的目标要被剔掉，不能让它占着第一位白打一次。
func TestGroupOrderDropsDeadTarget(t *testing.T) {
	cfg := groupTestConfig("workbuddy/not-exist", "workbuddy/glm-5.3")
	got := groupOrder(cfg, cfg.Groups[0])
	if len(got) != 1 || got[0] != "workbuddy/glm-5.3" {
		t.Fatalf("失效目标应被剔除，实际 %v", got)
	}
}

// 全部目标都失效时不能返回空 —— 空会让上层以为「不是路由组」，
// 从而按字面 group/xxx 去解析，报出一个更莫名其妙的错。
func TestGroupOrderAllDeadFallsBackToRawOrder(t *testing.T) {
	cfg := groupTestConfig("workbuddy/not-exist", "workbuddy/also-not-exist")
	got := groupOrder(cfg, cfg.Groups[0])
	if len(got) != 2 {
		t.Fatalf("全失效时应退回原始顺序，实际 %v", got)
	}
}

// 休息表：登记过的目标在被跳过，到点自动恢复。
func TestGroupRestSkipsThenRecovers(t *testing.T) {
	cfg := groupTestConfig("workbuddy/glm-5.3", "workbuddy/deepseek-v4.1-flash")
	g := cfg.Groups[0]

	groupRestPut("g1", "workbuddy/glm-5.3", 0) // 立即过期
	got := groupOrder(cfg, g)
	if len(got) != 2 {
		t.Fatalf("休息已过期，应恢复两个目标，实际 %v", got)
	}

	groupRestPut("g1", "workbuddy/glm-5.3", time.Hour)
	got = groupOrder(cfg, g)
	if len(got) != 1 || got[0] != "workbuddy/deepseek-v4.1-flash" {
		t.Fatalf("休息中的目标应被跳过，实际 %v", got)
	}
}

// groupTargetsFor：开关关掉就完全不生效。
func TestGroupTargetsForRespectsToggle(t *testing.T) {
	cfg := groupTestConfig("workbuddy/glm-5.3")
	if got, _ := groupTargetsFor(cfg, "group/g1"); len(got) == 0 {
		t.Fatal("开启时应解析出目标")
	}
	cfg.Routing.GroupsEnabled = false
	if got, _ := groupTargetsFor(cfg, "group/g1"); len(got) != 0 {
		t.Fatalf("关闭后不应生效，实际 %v", got)
	}
	// 非 group 前缀必须原样放过，不能被误判
	if got, _ := groupTargetsFor(cfg, "workbuddy/glm-5.3"); len(got) != 0 {
		t.Fatal("普通模型名不该被当成路由组")
	}
}
