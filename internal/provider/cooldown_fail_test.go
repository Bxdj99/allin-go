package provider

import (
	"testing"
	"time"

	"allin/internal/config"
)

// 429「额度已用尽」必须走长期冷却，不能按限流每 2 分钟重试。
// 实测线上 451 次 429 全部是 14018，旧行为导致每 2 分钟重打一次。
func TestFailWithBody_CreditExhaustedCoolsLong(t *testing.T) {
	p := NewPool()
	body := `{"error":{"data":{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度"}}}`

	p.FailWithBody("workbuddy", "acc", 429, "429", body)

	st := p.state["workbuddy/acc"]
	if st == nil {
		t.Fatal("状态未创建")
	}
	remain := time.Until(st.coolUntil)
	if remain < time.Hour {
		t.Errorf("额度用尽应长期冷却，实际只剩 %v（旧行为是 2 分钟）", remain)
	}
	if st.lastFailure != "额度已用尽" {
		t.Errorf("lastFailure = %q, want 额度已用尽", st.lastFailure)
	}
}

// 真正的窗口限流：冷却到上游给的重置时刻。
func TestFailWithBody_WindowResetsAtGivenTime(t *testing.T) {
	p := NewPool()
	// 给一个两小时后重置的时刻
	reset := time.Now().Add(2 * time.Hour).In(time.FixedZone("CST", 8*3600))
	body := `{"error":{"message":"您的使用量已超出频率限制，将在 ` +
		reset.Format("2006-01-02 15:04:05") + ` UTC+8 恢复"}}`

	p.FailWithBody("workbuddy", "acc", 429, "429", body)

	st := p.state["workbuddy/acc"]
	remain := time.Until(st.coolUntil)
	// 允许几秒误差
	if remain < 90*time.Minute || remain > 3*time.Hour {
		t.Errorf("应按上游重置时刻冷却（约 2 小时），实际 %v", remain)
	}
	if st.lastFailure != "频率限制，等待窗口重置" {
		t.Errorf("lastFailure = %q", st.lastFailure)
	}
}

// 无法识别的 429 退回默认短冷却，不能因为解析失败就冻很久。
func TestFailWithBody_UnknownRateLimitFallsBackShort(t *testing.T) {
	p := NewPool()
	p.FailWithBody("workbuddy", "acc", 429, "429", `{"error":{"message":"too many requests"}}`)

	st := p.state["workbuddy/acc"]
	remain := time.Until(st.coolUntil)
	if remain > 10*time.Minute {
		t.Errorf("无法识别时应短冷却，实际 %v", remain)
	}
}

// 额度用尽的账号不应被选中，避免每轮都白打一次上游。
func TestPickSkipsCreditExhaustedAccount(t *testing.T) {
	p := NewPool()
	enabled := true
	prov := &config.Provider{
		ID:      "workbuddy",
		Enabled: &enabled,
		Accounts: []*config.Account{
			{ID: "dead", Type: "token", Access: "t1", UID: "u1", Enabled: &enabled},
			{ID: "good", Type: "token", Access: "t2", UID: "u2", Enabled: &enabled},
		},
	}

	p.FailWithBody("workbuddy", "dead", 429, "429",
		`{"error":{"data":{"code":14018,"msg":"额度已用尽"}}}`)

	got := p.Pick(prov, nil)
	if got == nil {
		t.Fatal("应当选中 good")
	}
	if got.ID != "good" {
		t.Errorf("选中的是 %q，额度用尽的 dead 应被跳过", got.ID)
	}
}
