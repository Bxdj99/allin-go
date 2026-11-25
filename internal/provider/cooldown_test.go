package provider

import (
	"testing"
	"time"
)

// 429 有两种成因，处理方式必须不同。实测 451 次 429 全部是「额度已用尽」，
// 那是不会自愈的状态 —— 按限流每 2 分钟重试毫无意义。

func TestIsCreditExhausted(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "实测原文：14018 额度已用尽",
			body: `{"error":{"data":{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度：https://www.codebuddy.cn/profile/usage ","requestId":"b21aa0d2"}}}`,
			want: true,
		},
		{
			name: "码在顶层",
			body: `{"code":14018,"msg":"额度已用尽"}`,
			want: true,
		},
		{
			name: "字符串形态",
			body: `{"code":"credits_exhausted"}`,
			want: true,
		},
		{
			name: "只有文案",
			body: `{"msg":"额度已用尽，请购买加量包"}`,
			want: true,
		},
		{
			name: "真正的窗口限流",
			body: `{"error":{"message":"您的使用量已超出频率限制，将在 2026-11-26 08:00:00 UTC+8 恢复"}}`,
			want: false,
		},
		{
			name: "无关错误",
			body: `{"code":500,"msg":"internal error"}`,
			want: false,
		},
		{
			name: "空响应体",
			body: ``,
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsCreditExhausted(c.body); got != c.want {
				t.Errorf("IsCreditExhausted = %v, want %v", got, c.want)
			}
		})
	}
}

// 额度用尽不能返回窗口时间：等窗口没用，直接按长期冷却处理。
func TestQuotaWindowResetAt_ExhaustedIsNotWindow(t *testing.T) {
	body := `{"error":{"data":{"code":14018,"msg":"额度已用尽"}}}`
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	if got := QuotaWindowResetAt(body, now); !got.IsZero() {
		t.Errorf("额度用尽不应给出窗口重置时刻，得到 %v", got)
	}
}

func TestQuotaWindowResetAt_Chinese(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	body := `{"error":{"message":"您的使用量已超出频率限制，将在 2026-11-25 22:00:00 UTC+8 恢复"}}`

	got := QuotaWindowResetAt(body, now)
	if got.IsZero() {
		t.Fatal("应当解析出重置时刻")
	}
	// UTC+8 的 22:00 == UTC 14:00，距 now 两小时
	want := time.Date(2026, 11, 25, 22, 0, 0, 0, time.FixedZone("x", 8*3600))
	if !got.Equal(want) {
		t.Errorf("重置时刻 = %v, want %v", got.UTC(), want.UTC())
	}
	if !got.After(now) {
		t.Error("重置时刻必须晚于现在")
	}
}

func TestQuotaWindowResetAt_English(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	body := `{"message":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-11-25 22:00:00 UTC+8"}`

	got := QuotaWindowResetAt(body, now)
	if got.IsZero() {
		t.Fatal("应当解析出重置时刻")
	}
	if got.Sub(now) <= 0 {
		t.Errorf("重置时刻应晚于现在，差值 %v", got.Sub(now))
	}
}

// 已经过去的时间不能当窗口，否则账号会立刻解冻导致死循环重试。
func TestQuotaWindowResetAt_PastIgnored(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	body := `{"error":{"message":"您的使用量已超出频率限制，将在 2026-11-25 01:00:00 UTC+8 恢复"}}`
	if got := QuotaWindowResetAt(body, now); !got.IsZero() {
		t.Errorf("过去的时间不应作为窗口，得到 %v", got)
	}
}

// 解析出离谱的远未来时宁可放弃，不要锁死账号。
func TestQuotaWindowResetAt_TooFarIgnored(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	body := `{"error":{"message":"您的使用量已超出频率限制，将在 2027-11-25 20:00:00 UTC+8 恢复"}}`
	if got := QuotaWindowResetAt(body, now); !got.IsZero() {
		t.Errorf("超过 24 小时的窗口不应采信，得到 %v", got)
	}
}

// 文案不匹配时返回零值，调用方退回默认短冷却。
func TestQuotaWindowResetAt_Unrecognized(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	body := `{"error":{"message":"too many requests"}}`
	if got := QuotaWindowResetAt(body, now); !got.IsZero() {
		t.Errorf("无法识别时应返回零值，得到 %v", got)
	}
}
