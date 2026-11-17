package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

func mkProv(mode string, ids ...string) *config.Provider {
	p := &config.Provider{ID: "wb", Kind: "workbuddy", RouteMode: mode}
	for _, id := range ids {
		p.Accounts = append(p.Accounts, &config.Account{ID: id, Access: "k-" + id})
	}
	return p
}

// 会话绑定：同一会话连续请求必须落到同一账号
func TestStickySessionBindsAccount(t *testing.T) {
	p := NewPool()
	pr := mkProv("", "a", "b", "c") // 默认 sticky
	for _, id := range []string{"a", "b", "c"} {
		p.SetCredits("wb", id, 100, time.Now().Add(24*time.Hour))
	}

	key := SessionKey("wb", "session-1")
	first := p.PickForSession(pr, nil, key)
	if first == nil {
		t.Fatal("应选出账号")
	}
	for i := 0; i < 10; i++ {
		got := p.PickForSession(pr, nil, key)
		if got.ID != first.ID {
			t.Fatalf("同一会话应固定在同一账号，%s -> %s", first.ID, got.ID)
		}
		p.Success("wb", got.ID)
	}
}

// 不同会话应能分到不同账号
func TestStickyDifferentSessionsSpread(t *testing.T) {
	p := NewPool()
	pr := mkProv("", "a", "b", "c")
	for _, id := range []string{"a", "b", "c"} {
		p.SetCredits("wb", id, 100, time.Now().Add(24*time.Hour))
	}
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		k := SessionKey("wb", "sess-"+string(rune('A'+i)))
		if a := p.PickForSession(pr, nil, k); a != nil {
			seen[a.ID] = true
		}
	}
	if len(seen) < 2 {
		t.Fatalf("不同会话应分散到多个账号，实际只用了 %v", seen)
	}
}

// 绑定账号余额跌破下限时要解绑并换号
func TestStickyRebindsWhenLowCredits(t *testing.T) {
	p := NewPool()
	pr := mkProv("", "a", "b")
	key := SessionKey("wb", "s1")

	p.SetCredits("wb", "a", 100, time.Now().Add(24*time.Hour))
	p.SetCredits("wb", "b", 100, time.Now().Add(24*time.Hour))
	first := p.PickForSession(pr, nil, key)

	// 让第一个账号余额见底
	p.SetCredits("wb", first.ID, 0.1, time.Now().Add(24*time.Hour))

	next := p.PickForSession(pr, nil, key)
	if next == nil {
		t.Fatal("应换到其它账号")
	}
	if next.ID == first.ID {
		t.Fatal("余额跌破下限后应换号")
	}
}

// 轮询模式：连续请求应分散
func TestRotateModeSpreads(t *testing.T) {
	p := NewPool()
	pr := mkProv(config.RouteRotate, "a", "b")
	for _, id := range []string{"a", "b"} {
		p.SetCredits("wb", id, 100, time.Now().Add(24*time.Hour))
	}
	key := SessionKey("wb", "same")
	count := map[string]int{}
	for i := 0; i < 6; i++ {
		a := p.PickForSession(pr, nil, key)
		count[a.ID]++
		p.Success("wb", a.ID)
	}
	if count["a"] == 0 || count["b"] == 0 {
		t.Fatalf("轮询模式应分散，实际 %v", count)
	}
}

// 到期优先：最早到期的最先被选
func TestExpiryModePicksSoonest(t *testing.T) {
	p := NewPool()
	pr := mkProv(config.RouteExpiry, "later", "soon")
	p.SetCredits("wb", "later", 100, time.Now().Add(20*24*time.Hour))
	p.SetCredits("wb", "soon", 100, time.Now().Add(1*time.Hour))

	if a := p.PickForSession(pr, nil, ""); a.ID != "soon" {
		t.Fatalf("到期优先应选 soon，实际 %s", a.ID)
	}
}

// 用量均衡：余额比例低的优先
func TestBalanceModePicksLowestPct(t *testing.T) {
	p := NewPool()
	pr := mkProv(config.RouteBalance, "full", "low")
	p.SetCreditsFull("wb", "full", 90, 100, time.Now().Add(24*time.Hour)) // 90%
	p.SetCreditsFull("wb", "low", 10, 100, time.Now().Add(24*time.Hour))  // 10%

	if a := p.PickForSession(pr, nil, ""); a.ID != "low" {
		t.Fatalf("用量均衡应选比例最低的 low，实际 %s", a.ID)
	}
}
