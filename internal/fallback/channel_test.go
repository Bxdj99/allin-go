package fallback

import "testing"

// 真机截图里的原文
const realBlocked = `400 {"code":11128,"msg":"Illegal API invocation from an unapproved channel","requestId":"36ad11eb-49d7-4b43-98d4-173b7420e5f1","retry_policy":{"retry":false},"displayMsg":{"en":"Request blocked. Please send it again"}}`

func TestIsChannelBlocked(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{realBlocked, true},
		{`{"msg":"Illegal API invocation from an unapproved channel"}`, true},
		{`{"error":"channel verification failed"}`, true},
		{`{"code":6004,"msg":"quota exhausted"}`, false},
		{`{"code":10001,"msg":"already checked in"}`, false},
		{`{"error":"model not found"}`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := IsChannelBlocked(c.body); got != c.want {
			t.Errorf("IsChannelBlocked(%.40q) = %v, 期望 %v", c.body, got, c.want)
		}
	}
}

// 渠道拦截不该被误判为「换模型有用」
func TestChannelBlockedIsNotModelFallback(t *testing.T) {
	if ShouldFallbackModel(400, realBlocked) {
		t.Error("渠道拦截不应触发模型降级（换模型同样会失败）")
	}
}

func TestChannelBlockedHintNotEmpty(t *testing.T) {
	if ChannelBlockedHint() == "" {
		t.Fatal("提示文案不能为空")
	}
}
