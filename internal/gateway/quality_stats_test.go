package gateway

import (
	"sync/atomic"
	"testing"
)

// 请求质量统计：把「额度耗尽」从真错误里摘出来。
// 之前只有一个 Errors 总数，451 次额度 429 和真实故障混在一起，
// 完全判断不出严重程度。

func TestClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		auth   bool
		rate   bool
		ctx    bool
		want   string
	}{
		{
			name:   "额度耗尽（实测原文）",
			status: 429,
			body:   `{"error":{"data":{"code":14018,"msg":"额度已用尽"}}}`,
			rate:   true,
			want:   errQuota,
		},
		{
			name:   "窗口限流",
			status: 429,
			body:   `{"error":{"message":"您的使用量已超出频率限制"}}`,
			rate:   true,
			want:   errRateLimit,
		},
		{
			name:   "凭据失效 401",
			status: 401,
			body:   `{"msg":"unauthorized"}`,
			auth:   true,
			want:   errAuth,
		},
		{
			name:   "凭据失效 403",
			status: 403,
			body:   `<html>forbidden</html>`,
			auth:   true,
			want:   errAuth,
		},
		{
			name:   "上下文超限",
			status: 400,
			body:   `{"msg":"context too long"}`,
			ctx:    true,
			want:   errContext,
		},
		{
			name:   "上游 5xx",
			status: 502,
			body:   `{"msg":"bad gateway"}`,
			want:   errUpstream,
		},
		{
			name:   "网络层失败",
			status: 0,
			body:   ``,
			want:   errNetwork,
		},
		{
			name:   "其它 4xx",
			status: 422,
			body:   `{"msg":"bad param"}`,
			want:   errOther,
		},
		{
			name:   "成功不归类",
			status: 200,
			body:   `{}`,
			want:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyUpstreamError(c.status, c.body, c.auth, c.rate, c.ctx)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// 额度耗尽与限流不算「真实错误」—— 它们不是 bug，是账号状态。
func TestRealErrorsExcludesQuotaAndRateLimit(t *testing.T) {
	q := &QualityStats{errCnts: map[string]*atomic.Int64{}}
	q.RecordError(errQuota)
	q.RecordError(errQuota)
	q.RecordError(errRateLimit)
	q.RecordError(errAuth)
	q.RecordError(errUpstream)

	snap := q.Snapshot()
	if snap.Errors[errQuota] != 2 {
		t.Errorf("quota 应记 2，得到 %d", snap.Errors[errQuota])
	}
	// quota 2 + ratelimit 1 不算，剩 auth 1 + upstream 1
	if snap.RealErrors != 2 {
		t.Errorf("真实错误应为 2（auth + upstream），得到 %d", snap.RealErrors)
	}
}

// 裁剪统计：记录省下的 token 与占比。
func TestRecordTrim(t *testing.T) {
	q := &QualityStats{errCnts: map[string]*atomic.Int64{}}
	q.RecordTrim(100000, 60000) // 省 4 万
	q.RecordTrim(50000, 50000)  // 没省

	snap := q.Snapshot()
	if snap.TrimHits != 2 {
		t.Errorf("TrimHits = %d, want 2", snap.TrimHits)
	}
	if snap.TrimSaved != 40000 {
		t.Errorf("TrimSaved = %d, want 40000", snap.TrimSaved)
	}
	if snap.TrimBefore != 150000 {
		t.Errorf("TrimBefore = %d, want 150000", snap.TrimBefore)
	}
	// 40000/150000 ≈ 0.2667
	if snap.TrimRatio < 0.26 || snap.TrimRatio > 0.27 {
		t.Errorf("TrimRatio = %v, want ~0.2667", snap.TrimRatio)
	}
}

// 平均尝试次数：>1.5 说明换号频繁，值得关注。
func TestAvgAttempts(t *testing.T) {
	q := &QualityStats{errCnts: map[string]*atomic.Int64{}}
	for i := 0; i < 4; i++ {
		q.RecordRequest()
	}
	// 4 个请求共打了 6 次上游 → 平均 1.5
	for i := 0; i < 6; i++ {
		q.RecordAttempt()
	}
	if got := q.Snapshot().AvgAttempts; got != 1.5 {
		t.Errorf("AvgAttempts = %v, want 1.5", got)
	}
}

// 除零保护。
func TestQualityEmptySnapshot(t *testing.T) {
	q := &QualityStats{errCnts: map[string]*atomic.Int64{}}
	snap := q.Snapshot()
	if snap.AvgAttempts != 0 {
		t.Errorf("没有请求时 AvgAttempts 应为 0，得到 %v", snap.AvgAttempts)
	}
	if snap.TrimRatio != 0 {
		t.Errorf("没有裁剪时 TrimRatio 应为 0，得到 %v", snap.TrimRatio)
	}
}

// Reset 清空。
func TestQualityReset(t *testing.T) {
	q := &QualityStats{errCnts: map[string]*atomic.Int64{}}
	q.RecordError(errAuth)
	q.RecordTrim(1000, 500)
	q.RecordRequest()
	q.RecordAttempt()

	q.Reset()
	snap := q.Snapshot()
	if len(snap.Errors) != 0 || snap.TrimHits != 0 || snap.Requests != 0 || snap.Attempts != 0 {
		t.Errorf("Reset 后应全部归零，得到 %+v", snap)
	}
}
