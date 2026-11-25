package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 上游 AccountRoutingPolicy 的核心语义：
//
//	「够不够用」是确定事实 —— 余额明确低于门槛就是不够，与覆盖率无关；
//	「谁更富余」是相对排序 —— 只有全部候选都有新鲜读数时才可比。
//
// 旧实现把两者混在一起，导致「A 有新鲜快照、B 快照过期」时 A 无条件压过 B，
// 等于把「未知」当成「比 A 少」。积分刷新一断就把流量全压到少数几个号上。

// 覆盖率不完整时，不能因为「有读数」就优先于「未知」。
func TestNoPreferenceWhenCoverageIncomplete(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("known"), acct("unknown"))
	pr.RouteMode = config.RouteRotate

	// known 有新鲜快照、余额很充裕；unknown 从未刷新过
	p.SetCredits(pid, "known", 1000, time.Now().Add(time.Hour))

	// 两个号应当被轮转使用，而不是把请求全压给 known
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		a := p.PickForSession(pr, nil, "")
		if a == nil {
			t.Fatal("应能选出账号")
		}
		seen[a.ID] = true
		p.Success(pid, a.ID)
	}
	if !seen["unknown"] {
		t.Error("余额未知的账号不应因为「查不到」而被长期冷落")
	}
}

// 全部候选都有新鲜读数时，balance 策略才拿出余额比例来排序。
//
// balance 的语义是「剩余比例低的先上」，让各号消耗速度趋同 ——
// 所以选中 poor（剩余 3%）是对的，不是选中 rich。
// 这条测的是「覆盖率完整时才比较」，不是「选余额多的」。
func TestPreferLowerRatioWhenCoverageComplete(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("poor"), acct("rich"))
	pr.RouteMode = config.RouteBalance

	// balance 比的是剩余比例，必须给 total，否则比例算不出来
	p.SetCreditsFull(pid, "poor", 30, 1000, time.Now().Add(time.Hour))
	p.SetCreditsFull(pid, "rich", 900, 1000, time.Now().Add(time.Hour))

	a := p.PickForSession(pr, nil, "")
	if a == nil {
		t.Fatal("应能选出账号")
	}
	if a.ID != "poor" {
		t.Errorf("balance 策略应优先剩余比例低的账号，实际选中 %q", a.ID)
	}
}

// 余额明确低于门槛时，即使覆盖率不完整也应被剔除（确定事实，与覆盖率无关）。
func TestDefinitelyLowSkippedEvenWithoutCoverage(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("low"), acct("unknown"))
	pr.RouteMode = config.RouteRotate
	pr.RouteMode = config.RouteRotate

	p.SetCredits(pid, "low", 1, time.Now().Add(time.Hour)) // 低于默认门槛
	p.SetCreditReserve(true, 50, false)                    // 非严格模式

	for i := 0; i < 5; i++ {
		a := p.PickForSession(pr, nil, "")
		if a == nil {
			t.Fatal("应能选出账号")
		}
		if a.ID == "low" {
			t.Fatal("余额 1 明确低于门槛 50，即使其它号未知也不该选它")
		}
		p.Success(pid, a.ID)
	}
}

// 非严格模式：全部低于门槛时仍要给一个，不能让用户看到「全部不可用」。
func TestNonStrictFallsBackWhenAllLow(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))
	pr.RouteMode = config.RouteRotate

	p.SetCredits(pid, "a", 5, time.Now().Add(time.Hour))
	p.SetCredits(pid, "b", 8, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 50, false)

	if a := p.PickForSession(pr, nil, ""); a == nil {
		t.Fatal("非严格模式全部低于保底时仍应返回一个账号")
	}
}

// 严格模式：全部低于门槛就一个都不给。
func TestStrictReturnsNilWhenAllLow(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))
	pr.RouteMode = config.RouteRotate

	p.SetCredits(pid, "a", 5, time.Now().Add(time.Hour))
	p.SetCredits(pid, "b", 8, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 50, true)

	if a := p.PickForSession(pr, nil, ""); a != nil {
		t.Errorf("严格模式全部低于保底时应拒绝派活，实际选中 %q", a.ID)
	}
}
