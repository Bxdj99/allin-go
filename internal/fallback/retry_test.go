package fallback

import "testing"

func TestRetryForbidden(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"code":11148,"retry_policy":{"retry":false}}`, true},
		{`{"retry_policy": {"retry": false}}`, true},
		{`{"retry_policy":{"retry":true}}`, false},
		{`{"msg":"ok"}`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := RetryForbidden(c.body); got != c.want {
			t.Errorf("RetryForbidden(%q) = %v, 期望 %v", c.body, got, c.want)
		}
	}
}

// 真实响应原文（截图里那次的完整结构）
func TestRetryForbiddenRealBody(t *testing.T) {
	body := `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","requestId":"x","retry_policy":{"retry":false},"displayMsg":{"en":"Request blocked"}}`
	if !RetryForbidden(body) {
		t.Fatal("应识别为不可重试")
	}
	if !IsChannelBlocked(body) {
		t.Fatal("同时应识别为渠道拦截")
	}
}
