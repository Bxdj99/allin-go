package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 端到端验证：跑过重活的账号会被排到后面。
//
// 这是本次改动的核心目的。线上两个账号，202 次请求吃掉 7443 万输入 token，
// 1028 次请求吃掉 2.39 亿 —— 次数差 5 倍、消耗接近，
// 只按成功次数轮转会把重活反复分给同一个号。

func TestHeavyAccountGetsDeprioritized(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("heavy"), acct("light"))
	pr.RouteMode = config.RouteRotate

	// heavy 刚跑过一次 50 万 token 的重活
	p.ChargeLoad(pid, "heavy", 500000)

	// 后续请求应当优先给 light
	for i := 0; i < 5; i++ {
		a := p.PickForSession(pr, nil, "")
		if a == nil {
			t.Fatal("应能选出账号")
		}
		if a.ID == "heavy" {
			t.Fatalf("第 %d 次选中了重负载账号 heavy（负载 %v）", i+1, p.LoadOf(pid, "heavy"))
		}
		p.Success(pid, a.ID)
		p.ChargeLoad(pid, a.ID, 2000) // light 也在涨，但远小于 heavy
	}
}

// 两个账号负载接近时应正常轮转，不能被调度器锁死在一个号上。
func TestSimilarLoadStillRotates(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))
	pr.RouteMode = config.RouteRotate

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		acc := p.PickForSession(pr, nil, "")
		if acc == nil {
			t.Fatal("应能选出账号")
		}
		seen[acc.ID] = true
		p.Success(pid, acc.ID)
		// 两边各加相同量，保持负载持平
		p.ChargeLoad(pid, acc.ID, 1000)
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("负载持平时应轮转使用，实际只用了 %v", seen)
	}
}

// 负载差异小于阈值时视为持平，继续走原有轮转。
func TestLoadWithinTieKeepsRotation(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))
	pr.RouteMode = config.RouteRotate

	// 差异小于 loadTie
	p.ChargeLoad(pid, "a", loadTie*0.3)

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		acc := p.PickForSession(pr, nil, "")
		if acc == nil {
			t.Fatal("应能选出账号")
		}
		seen[acc.ID] = true
		p.Success(pid, acc.ID)
	}
	if !seen["a"] {
		t.Error("微小负载差异不应让账号被完全冷落")
	}
}

// 衰减后重负载账号应当重新回到候选。
func TestHeavyAccountRecoversAfterDecay(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("heavy"), acct("light"))
	pr.RouteMode = config.RouteRotate

	// 用一个可控时钟替换调度器的时间源
	now := time.Now()
	p.load.now = func() time.Time { return now }

	p.ChargeLoad(pid, "heavy", 1_000_000)

	// 刚记完：heavy 排在后面
	if a := p.PickForSession(pr, nil, ""); a == nil || a.ID != "light" {
		t.Fatalf("重负载时应选 light，实际 %v", a)
	}

	// 推进 10 个半衰期，重负载基本归零
	now = now.Add(10 * loadHalfLife)
	got := p.PickForSession(pr, nil, "")
	if got == nil {
		t.Fatal("应能选出账号")
	}
	// 此时两者负载接近，轮转应当能轮到 heavy
	seen := map[string]bool{got.ID: true}
	for i := 0; i < 6; i++ {
		a := p.PickForSession(pr, nil, "")
		if a == nil {
			t.Fatal("应能选出账号")
		}
		seen[a.ID] = true
		p.Success(pid, a.ID)
	}
	if !seen["heavy"] {
		t.Errorf("衰减后重负载账号应重新可用，实际只用过 %v", seen)
	}
}
