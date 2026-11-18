package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 401 连续 3 次应升格为长期冷却。
// 否则每轮积分钟刷新只是把 30 分钟重新点一次，死号永远显示「冷却中」却不恢复，
// 用户也无从判断这个号已经废了。
func TestDeadCredentialLongCool(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("dead"), acct("ok"))
	pr.RouteMode = config.RouteRotate

	p.Fail(pid, "dead", 401, "auth failed")
	p.Fail(pid, "dead", 401, "auth failed")
	p.Fail(pid, "dead", 401, "auth failed") // 第三次升格

	st := p.state[key(pid, "dead")]
	if remain := time.Until(st.coolUntil); remain < 6*24*time.Hour {
		t.Fatalf("第三次 401 应冷却约 7 天，实际剩余 %v", remain)
	}
}

// 前两次仍是普通冷却，不能一上来就判死
func TestDeadCredentialNotPremature(t *testing.T) {
	p := NewPool()
	p.Fail(pid, "a", 401, "auth failed")
	st := p.state[key(pid, "a")]
	if remain := time.Until(st.coolUntil); remain > 2*time.Hour {
		t.Fatalf("首次 401 不该直接长期冷却，实际 %v", remain)
	}
}
