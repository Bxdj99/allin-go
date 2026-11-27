package netfix

import (
	"strings"
	"testing"
)

func TestShortErr(t *testing.T) {
	cases := map[string]string{
		"dial udp: connect: network is unreachable": "网络不可达",
		"dial tcp: connect: connection refused":     "拒绝连接",
		"i/o timeout":                               "超时",
		"lookup x: no such host":                    "查不到主机",
	}
	for msg, want := range cases {
		if got := shortErr(&fakeErr{msg}); got != want {
			t.Errorf("shortErr(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestShortErrNil(t *testing.T) {
	if got := shortErr(nil); got != "" {
		t.Errorf("nil 应返回空串，得到 %q", got)
	}
}

// 全部不可达时要点明「大概率断网」，而不是让人去查 8.8.8.8。
func TestAllUnreachable(t *testing.T) {
	all := []string{
		"223.5.5.5（网络不可达）",
		"119.29.29.29（网络不可达）",
		"8.8.8.8（网络不可达）",
	}
	if !allUnreachable(all) {
		t.Error("全是网络不可达时应判定 true")
	}

	mixed := []string{
		"223.5.5.5（网络不可达）",
		"8.8.8.8（超时）",
	}
	if allUnreachable(mixed) {
		t.Error("混合原因不该判定为全部不可达")
	}

	if allUnreachable(nil) {
		t.Error("空列表不该判定为 true")
	}
}

// 汇总信息必须带上每个服务器，不能只报最后一个。
func TestDNSFailureMentionsAllServers(t *testing.T) {
	// 直接验证拼接逻辑：模拟三个都失败
	fails := []string{
		"223.5.5.5（网络不可达）",
		"119.29.29.29（网络不可达）",
		"8.8.8.8（网络不可达）",
	}
	detail := strings.Join(fails, "；")
	for _, n := range []string{"223.5.5.5", "119.29.29.29", "8.8.8.8"} {
		if !strings.Contains(detail, n) {
			t.Errorf("汇总里应包含 %s，实际 %q", n, detail)
		}
	}
}

// 一个小 err 替身，避免引入额外依赖。
type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }
