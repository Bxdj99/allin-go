package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 保底关闭时，余额 5 的号仍可用（门槛 = CreditsFloor = 1）
func TestReserveOffAllowsLowBalance(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("low"), acct("rich"))
	pr.RouteMode = config.RouteRotate
	p.SetCredits(pid, "low", 5, time.Now().Add(time.Hour))
	p.SetCredits(pid, "rich", 100, time.Now().Add(time.Hour))

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		a := p.PickForSession(pr, nil, "")
		if a == nil {
			t.Fatal("应能选出账号")
		}
		seen[a.ID] = true
		p.Success(pid, a.ID)
	}
	if !seen["low"] {
		t.Fatal("保底关闭时余额 5 的号应能被选中")
	}
}

// 保底开启（20）时，余额 5 的号不应被选中
func TestReserveOnSkipsLowBalance(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("low"), acct("rich"))
	pr.RouteMode = config.RouteRotate
	p.SetCredits(pid, "low", 5, time.Now().Add(time.Hour))
	p.SetCredits(pid, "rich", 100, time.Now().Add(time.Hour))

	p.SetCreditReserve(true, 20, false)

	for i := 0; i < 4; i++ {
		a := p.PickForSession(pr, nil, "")
		if a == nil {
			t.Fatal("应能选出账号")
		}
		if a.ID == "low" {
			t.Fatal("余额 5 低于保底 20，不应被选中")
		}
		p.Success(pid, a.ID)
	}
}

// 保底值可自定义：设成 3 时余额 5 又能用了
func TestReserveAmountCustom(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("low"), acct("rich"))
	pr.RouteMode = config.RouteRotate
	p.SetCredits(pid, "low", 5, time.Now().Add(time.Hour))
	p.SetCredits(pid, "rich", 100, time.Now().Add(time.Hour))

	p.SetCreditReserve(true, 3, false)

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		a := p.PickForSession(pr, nil, "")
		seen[a.ID] = true
		p.Success(pid, a.ID)
	}
	if !seen["low"] {
		t.Fatal("保底设为 3 时，余额 5 应可用")
	}
}

// 全部账号都低于保底时，仍然要能选出一个（总比直接失败强）
func TestReserveAllBelowStillPicks(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))
	p.SetCredits(pid, "a", 5, time.Now().Add(time.Hour))
	p.SetCredits(pid, "b", 10, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 20, false)

	if a := p.PickForSession(pr, nil, ""); a == nil {
		t.Fatal("全部低于保底时也应返回一个账号，不能返回 nil")
	}
}

// 保底值不能把门槛降到 CreditsFloor 以下
func TestReserveAmountBelowFloor(t *testing.T) {
	p := NewPool()
	p.SetCreditReserve(true, 0.1, false)
	if got := p.creditFloor(); got != CreditsFloor {
		t.Fatalf("保底小于基础门槛时应取 %v，实际 %v", CreditsFloor, got)
	}
}
