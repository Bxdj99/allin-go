package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 连续 401 应标记「需重新登录」，而不是只显示冷却中
func TestNeedReloginFlag(t *testing.T) {
	p := NewPool()
	pr := prov(pid, acct("dead"), acct("ok"))
	pr.RouteMode = config.RouteRotate

	for i := 0; i < 3; i++ {
		p.Fail(pid, "dead", 401, "Unauthorized")
	}
	st := p.state[key(pid, "dead")]
	if !st.needRelogin {
		t.Fatal("连续 3 次 401 应标记 needRelogin")
	}

	// 面板快照要能读到
	rows := p.Snapshot()
	found := false
	for _, r := range rows {
		if r["key"] == key(pid, "dead") {
			if r["needRelogin"] == true {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("Snapshot 应输出 needRelogin")
	}
}

// 成功一次就要清掉标记
func TestNeedReloginClearedOnSuccess(t *testing.T) {
	p := NewPool()
	for i := 0; i < 3; i++ {
		p.Fail(pid, "a", 401, "Unauthorized")
	}
	if !p.state[key(pid, "a")].needRelogin {
		t.Fatal("前置：应已标记")
	}
	p.Success(pid, "a")
	if p.state[key(pid, "a")].needRelogin {
		t.Fatal("成功后应清除标记")
	}
}

// 手动标记也生效
func TestMarkNeedRelogin(t *testing.T) {
	p := NewPool()
	p.MarkNeedRelogin(pid, "x", true)
	st := p.state[key(pid, "x")]
	if !st.needRelogin {
		t.Fatal("应标记成功")
	}
	if time.Until(st.coolUntil) < 24*time.Hour {
		t.Fatal("标记时应同时设长期冷却")
	}
	p.MarkNeedRelogin(pid, "x", false)
	if p.state[key(pid, "x")].needRelogin {
		t.Fatal("应能取消标记")
	}
}
