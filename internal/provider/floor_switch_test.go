package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 用户要求：会话绑定后也要在余额 ≤ 门槛时换号，不能烧到 0。
//
// 这条针对的是「已绑定的会话」——最容易出问题的地方：
// 绑定优先于选号，如果 accountUsable 放行了低余额账号，
// 请求会一直打在这个号上直到彻底耗尽。

// 余额正好等于门槛（5）→ 该换。
func TestBoundSessionSwitchesAtExactFloor(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("low"), acct("rich"))
	pr.RouteMode = config.RouteSticky

	p.SetCredits(pid, "low", 5, time.Now().Add(time.Hour)) // 正好等于门槛
	p.SetCredits(pid, "rich", 500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 5, true) // 严格模式，门槛 5

	p.sessions.Bind("sess", "low")

	got := p.PickForSession(pr, nil, "sess")
	if got == nil {
		t.Fatal("应能选出账号")
	}
	if got.ID == "low" {
		t.Error("余额正好等于门槛 5 时应当换号，不能继续烧")
	}
}

// 余额 6（高于门槛）→ 继续用，不该换。
func TestBoundSessionKeepsAboveFloor(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("ok"), acct("rich"))
	pr.RouteMode = config.RouteSticky

	p.SetCredits(pid, "ok", 6, time.Now().Add(time.Hour))
	p.SetCredits(pid, "rich", 500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 5, true)

	p.sessions.Bind("sess", "ok")

	got := p.PickForSession(pr, nil, "sess")
	if got == nil {
		t.Fatal("应能选出账号")
	}
	if got.ID != "ok" {
		t.Errorf("余额 6 高于门槛 5，应继续用已绑定的 ok，实际 %q", got.ID)
	}
}

// 余额 0 → 必须换，这是最底线。
func TestBoundSessionSwitchesAtZero(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("dead"), acct("rich"))
	pr.RouteMode = config.RouteSticky

	p.SetCredits(pid, "dead", 0, time.Now().Add(time.Hour))
	p.SetCredits(pid, "rich", 500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 5, true)

	p.sessions.Bind("sess", "dead")

	got := p.PickForSession(pr, nil, "sess")
	if got == nil {
		t.Fatal("应能选出账号")
	}
	if got.ID == "dead" {
		t.Error("余额 0 绝对不能继续用")
	}
}

// 严格模式压过缓存保护：即使命中 99% 也要换。
func TestBoundSessionStrictBeatsCache(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("low"), acct("rich"))
	pr.RouteMode = config.RouteSticky

	p.SetCredits(pid, "low", 5, time.Now().Add(time.Hour))
	p.RecordCacheRate(pid, "low", 990000, 1000000, true) // 命中 99%
	p.SetCredits(pid, "rich", 500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 5, true)
	p.SetSwitchPolicy(true, 0.5, true) // 缓存保护开着

	p.sessions.Bind("sess", "low")

	got := p.PickForSession(pr, nil, "sess")
	if got == nil {
		t.Fatal("应能选出账号")
	}
	if got.ID == "low" {
		t.Error("严格模式下余额 5 必须换号，缓存保护不能拦住")
	}
}
