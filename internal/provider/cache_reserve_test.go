package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 保底 vs 缓存：换号会让整段输入从「按 10% 计价」变全价。
//
// 实测（2026-11-26，同一会话 54.9 万 token）：
//   留在原账号（余额 47，命中 99.9%）→ 等效约 5.5 万 token
//   切到新账号（余额 1500，命中 0%） → 实际 54.9 万 token
//
// 为保住 47 积分而多花 10 倍 token，明显是亏的。

func TestCacheKeepsAccountBelowReserve(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("hot"), acct("cold"))
	pr.RouteMode = config.RouteSticky

	// hot：余额低于保底，但缓存命中率很高
	p.SetCredits(pid, "hot", 47, time.Now().Add(time.Hour))
	p.RecordCacheRate(pid, "hot", 540000, 545000, true) // ~99%

	// cold：余额充足但没缓存数据
	p.SetCredits(pid, "cold", 1500, time.Now().Add(time.Hour))

	// 非严格模式：缓存保护生效（严格模式下会强制换号，那是另一个用例）
	p.SetCreditReserve(true, 50, false)

	// 先建立会话绑定到 hot
	p.sessions.Bind("sess-1", "hot")

	got := p.PickForSession(pr, nil, "sess-1")
	if got == nil {
		t.Fatal("应继续返回 hot，而不是拒绝派活")
	}
	if got.ID != "hot" {
		t.Errorf("缓存命中高时应保留 hot，实际切到 %q（会丢失 99%% 缓存）", got.ID)
	}
}

// 缓存命中率低时，保底照常换号。
func TestLowCacheStillSwitchesOnReserve(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("lowcache"), acct("rich"))
	pr.RouteMode = config.RouteSticky

	p.SetCredits(pid, "lowcache", 47, time.Now().Add(time.Hour))
	// 命中率只有 10%，换号损失不大
	p.RecordCacheRate(pid, "lowcache", 10000, 100000, true)
	p.SetCredits(pid, "rich", 1500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 50, true)

	p.sessions.Bind("sess-1", "lowcache")
	got := p.PickForSession(pr, nil, "sess-1")
	if got == nil {
		t.Fatal("应能选出账号")
	}
	if got.ID == "lowcache" {
		t.Error("命中率低时不该为保余额而牺牲换号机会")
	}
}

// 没有缓存数据时，保底按原逻辑换号。
func TestNoCacheDataKeepsReserveBehavior(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("nocache"), acct("rich"))
	pr.RouteMode = config.RouteSticky

	p.SetCredits(pid, "nocache", 10, time.Now().Add(time.Hour))
	p.SetCredits(pid, "rich", 1500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 50, true)

	p.sessions.Bind("sess-1", "nocache")
	got := p.PickForSession(pr, nil, "sess-1")
	if got == nil {
		t.Fatal("应能选出账号")
	}
	if got.ID == "nocache" {
		t.Error("没有缓存数据时不应特殊照顾")
	}
}

// CacheHealth 的正确性。
func TestCacheHealth(t *testing.T) {
	p := NewPool()
	if _, known := p.CacheHealth("wb", "x"); known {
		t.Error("没有数据时应 unknown")
	}
	p.RecordCacheRate("wb", "x", 900, 1000, true)
	rate, known := p.CacheHealth("wb", "x")
	if !known {
		t.Fatal("记过数据后应 known")
	}
	if rate < 0.89 || rate > 0.91 {
		t.Errorf("命中率应约 0.9，得到 %v", rate)
	}
}

// 未上报缓存字段的请求不参与统计。
func TestCacheRateIgnoresUnknown(t *testing.T) {
	p := NewPool()
	p.RecordCacheRate("wb", "y", 0, 1000, false) // known=false
	if _, known := p.CacheHealth("wb", "y"); known {
		t.Error("上游没报缓存字段时不该产生统计")
	}
}

// 严格模式压过缓存保护：用户开严格模式就是明确表态「宁可丢缓存也不动底仓」。
// 实测反馈：即使命中 99%，余额跌破门槛也必须换号。
func TestStrictModeOverridesCacheProtection(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("hot"), acct("rich"))
	pr.RouteMode = config.RouteSticky

	p.SetCredits(pid, "hot", 5, time.Now().Add(time.Hour))
	p.RecordCacheRate(pid, "hot", 540000, 545000, true) // 命中 99%
	p.SetCredits(pid, "rich", 500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 10, true) // 严格模式

	p.sessions.Bind("sess-1", "hot")
	got := p.PickForSession(pr, nil, "sess-1")
	if got == nil {
		t.Fatal("应能选出账号")
	}
	if got.ID == "hot" {
		t.Error("严格模式下余额 5 低于门槛 10，即使缓存命中 99% 也必须换号")
	}
}

// 非严格模式下，同样的场景应当保住缓存。
func TestNonStrictKeepsCacheAccount(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("hot"), acct("rich"))
	pr.RouteMode = config.RouteSticky

	p.SetCredits(pid, "hot", 5, time.Now().Add(time.Hour))
	p.RecordCacheRate(pid, "hot", 540000, 545000, true)
	p.SetCredits(pid, "rich", 500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 10, false) // 非严格

	p.sessions.Bind("sess-1", "hot")
	got := p.PickForSession(pr, nil, "sess-1")
	if got == nil {
		t.Fatal("应能选出账号")
	}
	if got.ID != "hot" {
		t.Errorf("非严格模式下应保住高命中率的 hot，实际 %q", got.ID)
	}
}
