package gateway

import "testing"

// isCredError 决定「要不要换账号重试」。
// 判错的两个方向都会伤人：
//   - 把真实结论误判成凭据错误 → 白打上游 5 次，还可能把「不允许」报成「401」
//   - 把凭据错误漏判 → 就是这个 bug 本身：一个坏号污染整页模型

func TestIsCredError(t *testing.T) {
	cases := []struct {
		name string
		in   ModelProbe
		want bool
	}{
		{"ok 不算凭据错", ModelProbe{OK: true, Status: "ok"}, false},

		// 明确业务码：真实结论，不该换号重试
		{"11158 不允许", ModelProbe{Status: "not_allowed", Code: 11158}, false},
		{"11102 本站无", ModelProbe{Status: "not_found", Code: 11102}, false},
		{"6004 限流", ModelProbe{Status: "rate_limited", Code: 6004}, false},
		{"4011 限流", ModelProbe{Status: "rate_limited", Code: 4011}, false},

		// 凭据坏：要换号
		{"HTTP 401", ModelProbe{Status: "error", Msg: "凭据失效（HTTP 401）"}, true},
		{"HTTP 403", ModelProbe{Status: "error", Msg: "forbidden 403"}, true},
		{"unauthorized", ModelProbe{Status: "error", Msg: "Unauthorized"}, true},
		{"token expired", ModelProbe{Status: "error", Msg: "token expired, please relogin"}, true},
		{"invalid token", ModelProbe{Status: "error", Msg: "invalid token"}, true},

		// 其它错误：不换号（网络/超时换号也救不回来，反而拖慢探测）
		{"超时", ModelProbe{Status: "error", Msg: "context deadline exceeded"}, false},
		{"空响应", ModelProbe{Status: "empty", Msg: "没有产出内容"}, false},
	}
	for _, c := range cases {
		if got := isCredError(c.in); got != c.want {
			t.Errorf("%s: isCredError(%+v) = %v, 期望 %v", c.name, c.in, got, c.want)
		}
	}
}

// 空候选列表不能 panic。
func TestProbeWithFallbackNoAccounts(t *testing.T) {
	mp := probeWorkBuddyWithFallback(nil, "workbuddy", nil, "hy3")
	if mp.OK {
		t.Error("没有账号时不该报可用")
	}
	if mp.Status != "error" {
		t.Errorf("状态应为 error，实际 %q", mp.Status)
	}
}
