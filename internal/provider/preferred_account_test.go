package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 优先账号：只要可用就一直用它。
//
// 理由是最省的用法 —— 换号会让上游前缀缓存失效，
// 整段输入从约 10% 计价变全价（实测同一会话 54.9 万 token 差 10 倍）。

func TestPreferredAccountAlwaysPicked(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("main"), acct("other"))
	pr.RouteMode = config.RouteRotate // 甚至会轮询，但优先账号应当压过它
	pr.PreferredAccountID = "main"

	// 连选 10 次都应是 main
	for i := 0; i < 10; i++ {
		a := p.PickForSession(pr, nil, "sess")
		if a == nil {
			t.Fatal("应能选出账号")
		}
		if a.ID != "main" {
			t.Fatalf("指定优先账号后应始终选 main，第 %d 次拿到 %q", i+1, a.ID)
		}
		p.Success(pid, a.ID)
	}
}

// 优先账号不可用（冷却）时退回普通策略，不能卡死。
func TestPreferredAccountFallsBackWhenCooling(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("main"), acct("backup"))
	pr.RouteMode = config.RouteSticky
	pr.PreferredAccountID = "main"

	// 让 main 进入长期冷却
	p.FailWithBody(pid, "main", 429, "429",
		`{"error":{"data":{"code":14018,"msg":"额度已用尽"}}}`)

	a := p.PickForSession(pr, nil, "sess")
	if a == nil {
		t.Fatal("应能退回 backup")
	}
	if a.ID != "backup" {
		t.Errorf("main 不可用时应选 backup，实际 %q", a.ID)
	}
}

// 余额低于门槛时也算不可用。
func TestPreferredAccountFallsBackWhenLowCredit(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("main"), acct("backup"))
	pr.RouteMode = config.RouteSticky
	pr.PreferredAccountID = "main"

	p.SetCredits(pid, "main", 3, time.Now().Add(time.Hour))
	p.SetCredits(pid, "backup", 500, time.Now().Add(time.Hour))
	p.SetCreditReserve(true, 10, false)

	a := p.PickForSession(pr, nil, "sess")
	if a == nil {
		t.Fatal("应能选出账号")
	}
	if a.ID != "backup" {
		t.Errorf("main 余额 3 低于门槛 10，应退回 backup，实际 %q", a.ID)
	}
}

// 没指定优先账号时行为不变。
func TestNoPreferredKeepsNormalRouting(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("a"), acct("b"))
	pr.RouteMode = config.RouteRotate
	// PreferredAccountID 留空

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		acc := p.PickForSession(pr, nil, "")
		if acc == nil {
			t.Fatal("应能选出账号")
		}
		seen[acc.ID] = true
		p.Success(pid, acc.ID)
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("未指定优先账号时应正常轮转，实际只用了 %v", seen)
	}
}

// ResetSessions 改规则后清空绑定，避免旧绑定把请求继续钉在原账号。
func TestResetSessions(t *testing.T) {
	p := NewPool()
	p.sessions.Bind("k1", "a")
	p.sessions.Bind("k2", "b")
	p.ResetSessions()
	if got := p.sessions.Lookup("k1"); got != "" {
		t.Errorf("Reset 后不应有绑定，得到 %q", got)
	}
	if got := p.sessions.Lookup("k2"); got != "" {
		t.Errorf("Reset 后不应有绑定，得到 %q", got)
	}
}

// CreditsOf 供面板展示余额。
func TestCreditsOf(t *testing.T) {
	p := NewPool()
	if _, ok := p.CreditsOf(pid, "x"); ok {
		t.Error("没有快照时应返回 false")
	}
	p.SetCredits(pid, "x", 123, time.Now().Add(time.Hour))
	v, ok := p.CreditsOf(pid, "x")
	if !ok || v != 123 {
		t.Errorf("应返回 123，得到 %v/%v", v, ok)
	}
}
