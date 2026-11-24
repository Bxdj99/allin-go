package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 严格模式：全部低于保底时返回 nil
func TestStrictReserveReturnsNil(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))
	pr.RouteMode = config.RouteRotate
	p.SetCredits(pid, "a", 5, time.Now().Add(time.Hour))
	p.SetCredits(pid, "b", 10, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 20, true) // 开启 + 严格

	if a := p.PickForSession(pr, nil, ""); a != nil {
		t.Fatalf("严格模式下全部低于保底应返回 nil，实际选了 %s", a.ID)
	}
}

// 非严格模式：仍能选出一个
func TestNonStrictReserveStillPicks(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))
	pr.RouteMode = config.RouteRotate
	p.SetCredits(pid, "a", 5, time.Now().Add(time.Hour))
	p.SetCredits(pid, "b", 10, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 20, false) // 开启但不严格

	if a := p.PickForSession(pr, nil, ""); a == nil {
		t.Fatal("非严格模式应仍能选出账号")
	}
}

// 严格模式但有余额充足的号，正常选它
func TestStrictReservePicksRichAccount(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("low"), acct("rich"))
	pr.RouteMode = config.RouteRotate
	p.SetCredits(pid, "low", 5, time.Now().Add(time.Hour))
	p.SetCredits(pid, "rich", 500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 20, true)

	for i := 0; i < 3; i++ {
		a := p.PickForSession(pr, nil, "")
		if a == nil {
			t.Fatal("有充足账号时应能选出")
		}
		if a.ID != "rich" {
			t.Fatalf("应选余额充足的，实际 %s", a.ID)
		}
		p.Success(pid, a.ID)
	}
}

// sticky 模式下的严格判断同样生效
func TestStrictReserveSticky(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"))
	// sticky 是默认模式
	p.SetCredits(pid, "a", 3, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 20, true)

	if a := p.PickForSession(pr, nil, "sess-1"); a != nil {
		t.Fatalf("sticky 模式下严格保底应拦住，实际选了 %s", a.ID)
	}
}

// 没有积分快照的账号不该被严格模式拦
func TestStrictReserveAllowsUnknownBalance(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"))
	// 不设积分快照
	p.SetCreditReserve(true, 20, true)

	if a := p.PickForSession(pr, nil, ""); a == nil {
		t.Fatal("没有积分快照时判断不了，不该被拦")
	}
}
