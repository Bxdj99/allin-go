package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// acct 造一个启用中的账号。
func acct(id string) *config.Account {
	return &config.Account{ID: id, Access: "token-" + id}
}

// prov 造一个含多个账号的供应商。
func prov(id string, accts ...*config.Account) *config.Provider {
	return &config.Provider{ID: id, Accounts: accts}
}

const pid = "workbuddy"

// 没有任何积分快照时，行为应与改动前一致：所有账号都参与 least-used 轮转。
func TestPickNoCreditsFallsBackToLeastUsed(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))
	pr.RouteMode = config.RouteRotate // 无绑定轮转语义

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		a := p.Pick(pr, nil)
		if a == nil {
			t.Fatal("Pick 返回 nil")
		}
		seen[a.ID]++
		p.Success(pid, a.ID)
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("无积分快照时应在账号间轮转，实际分布: %v", seen)
	}
}

// 余额足够但没快照的账号，不应因为「查不到积分」就被冷落。
func TestPickIgnoresStaleCredits(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))

	// a 的余额为 0，但快照已过期（超过 creditsStale），应视为无信息
	p.SetCredits(pid, "a", 0, time.Now().Add(time.Hour))
	p.state[key(pid, "a")].creditsAt = time.Now().Add(-2 * creditsStale)

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		a := p.Pick(pr, nil)
		seen[a.ID]++
		p.Success(pid, a.ID)
	}
	if seen["a"] == 0 {
		t.Fatalf("过期快照不应让账号被排除，实际: %v", seen)
	}
}

// 余额充足的账号必须优先于余额不足的账号。
func TestPickPrefersFundedAccount(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("rich"), acct("empty"))

	p.SetCredits(pid, "rich", 100, time.Now().Add(30*24*time.Hour))
	p.SetCredits(pid, "empty", 0, time.Now().Add(time.Hour))

	for i := 0; i < 3; i++ {
		a := p.Pick(pr, nil)
		if a.ID != "rich" {
			t.Fatalf("应优先选余额充足的账号，实际选了 %s", a.ID)
		}
		p.Success(pid, a.ID)
	}
}

// 余额低于 CreditsFloor 视为不够用。
func TestPickRespectsCreditsFloor(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("low"), acct("ok"))

	p.SetCredits(pid, "low", CreditsFloor-0.1, time.Now().Add(time.Hour))
	p.SetCredits(pid, "ok", 50, time.Now().Add(60*24*time.Hour))

	for i := 0; i < 3; i++ {
		a := p.Pick(pr, nil)
		if a.ID != "ok" {
			t.Fatalf("低于下限的账号不应被选中，实际选了 %s", a.ID)
		}
		p.Success(pid, a.ID)
	}
}

// 两个账号余额都充足、且用得一样多时，快到期（ResetAt 更近）的优先。
func TestPickPrefersSoonerResetOnTie(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("later"), acct("soon"))
	pr.RouteMode = config.RouteExpiry

	p.SetCredits(pid, "later", 100, time.Now().Add(20*24*time.Hour))
	p.SetCredits(pid, "soon", 5, time.Now().Add(24*time.Hour))

	// 两个号成功次数都是 0，是平局，此时按到期时间决定
	a := p.Pick(pr, nil)
	if a.ID != "soon" {
		t.Fatalf("平局时应优先用快过期的额度，实际选了 %s", a.ID)
	}
}

// 关键回归：最早到期的账号不能垄断选号，必须和其余账号轮流用。
// 否则「优先用快过期的」会退化成单账号，违背多账号轮转。
func TestPickRotatesInsteadOfMonopolizing(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("urgent"), acct("normal"))
	pr.RouteMode = config.RouteRotate

	// urgent 余额最多且最早到期，按旧逻辑会一直被选中
	p.SetCredits(pid, "urgent", 1000, time.Now().Add(24*time.Hour))
	p.SetCredits(pid, "normal", 10, time.Now().Add(20*24*time.Hour))

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		a := p.Pick(pr, nil)
		if a == nil {
			t.Fatal("Pick 返回 nil")
		}
		seen[a.ID]++
		p.Success(pid, a.ID)
	}
	if seen["urgent"] == 0 || seen["normal"] == 0 {
		t.Fatalf("两个号都该被用到（轮流用），实际分布: %v", seen)
	}
	// 负载应基本均衡，允许 1 的偏差
	if d := seen["urgent"] - seen["normal"]; d > 1 || d < -1 {
		t.Fatalf("负载应大致均衡，实际分布: %v", seen)
	}
}

// 到期时间未知的账号，在负载相同时应排在到期时间已知的后面。
func TestPickUnknownResetRanksLast(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("unknown"), acct("known"))

	p.SetCredits(pid, "unknown", 100, time.Time{})
	p.SetCredits(pid, "known", 3, time.Now().Add(2*24*time.Hour))

	// 两个号成功次数都是 0，是平局，此时到期已知的应胜出。
	// 只验一轮：多发几次会因 least-used 轮转而选中另一个，那是预期行为。
	// 用 expiry 策略验证「到期已知优先」，sticky 下会按负载选号
	pr.RouteMode = config.RouteExpiry
	if a := p.Pick(pr, nil); a.ID != "known" {
		t.Fatalf("负载相同时到期已知的应优先，实际选了 %s", a.ID)
	}
}

// 冷却中的账号不应被选中，即便它余额最充足、最紧急。
func TestPickSkipsCooling(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("hot"), acct("cool"))

	p.SetCredits(pid, "hot", 100, time.Now().Add(time.Hour))
	p.Fail(pid, "hot", 429, "rate limited")
	p.SetCredits(pid, "cool", 5, time.Now().Add(30*24*time.Hour))

	a := p.Pick(pr, nil)
	if a == nil || a.ID != "cool" {
		t.Fatalf("应跳过冷却中的账号，实际: %v", a)
	}
}

// 积分感知不应破坏换号重试：exclude 的账号必须被跳过。
func TestPickHonorsExclude(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))

	p.SetCredits(pid, "a", 100, time.Now().Add(time.Hour))
	p.SetCredits(pid, "b", 5, time.Now().Add(30*24*time.Hour))

	a := p.Pick(pr, map[string]bool{"a": true})
	if a == nil || a.ID != "b" {
		t.Fatalf("exclude 未生效，实际: %v", a)
	}
}

// 所有账号都在冷却时，仍要返回最早恢复的那个（不能返回 nil）。
func TestPickAllCoolingReturnsEarliest(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))

	p.Fail(pid, "a", 500, "boom")   // 1 分钟
	p.Fail(pid, "b", 429, "limit")  // 2 分钟

	a := p.Pick(pr, nil)
	if a == nil {
		t.Fatal("全部冷却时应返回最早恢复的账号，不能为 nil")
	}
	if a.ID != "a" {
		t.Fatalf("应返回最早恢复的账号 a，实际: %s", a.ID)
	}
}
