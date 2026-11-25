package gateway

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// 请求质量统计。
//
// 目的：把「积分花在哪」拆开看。之前只有一个 Errors 总数，
// 里面混着 429（额度问题，不算错误）、401（凭据问题）、真实故障，
// 完全看不出真实失败率。
//
// 另外两项：
//   - 裁剪：省了多少 token（用户问「上下文是不是被砍了」时的唯一依据）
//   - 重试：一次请求平均打了几次上游（>1.5 说明稳定性或选号有问题）

// 错误分类。用字符串而不是枚举，方便日志里直接读。
const (
	errQuota     = "quota"     // 429 额度耗尽（不是故障）
	errRateLimit = "ratelimit" // 429 窗口限流（不是故障）
	errAuth      = "auth"      // 401/403 凭据失效
	errContext   = "context"   // 上下文超限
	errChannel   = "channel"   // 11128 渠道拦截
	errUpstream  = "upstream"  // 5xx / 其它上游错误
	errNetwork   = "network"   // 连接层失败
	errToolPair  = "toolpair"  // 11148 工具配对
	errSignature = "signature" // thinking 签名
	errOther     = "other"
)

// QualityStats 是进程级的请求质量计数。全部原子操作，不阻塞请求路径。
type QualityStats struct {
	// Errors 按分类累计。
	errMu   sync.RWMutex
	errCnts map[string]*atomic.Int64

	// 裁剪统计：发生裁剪的请求数、省的 token 总量、裁剪前总量。
	TrimHits   atomic.Int64
	TrimSaved  atomic.Int64
	TrimBefore atomic.Int64

	// 重试统计：总尝试次数 与 需要的请求数。
	// 平均尝试次数 = Attempts / Requests。
	Attempts atomic.Int64
	Requests atomic.Int64

	// 换号/换模型次数（沿用 Server 里已有的计数器，这里只做补充）。
	AccountSwitches atomic.Int64
}

var quality = &QualityStats{errCnts: map[string]*atomic.Int64{}}

func (q *QualityStats) errCounter(kind string) *atomic.Int64 {
	q.errMu.RLock()
	c := q.errCnts[kind]
	q.errMu.RUnlock()
	if c != nil {
		return c
	}
	q.errMu.Lock()
	defer q.errMu.Unlock()
	if c = q.errCnts[kind]; c == nil {
		c = &atomic.Int64{}
		q.errCnts[kind] = c
	}
	return c
}

// RecordError 记一次分类错误。
func (q *QualityStats) RecordError(kind string) {
	if kind == "" {
		kind = errOther
	}
	q.errCounter(kind).Add(1)
	markQualityDirty()
}

// RecordTrim 记一次裁剪。
func (q *QualityStats) RecordTrim(before, after int) {
	if before <= 0 {
		return
	}
	if after < 0 {
		after = 0
	}
	q.TrimHits.Add(1)
	markQualityDirty()
	q.TrimBefore.Add(int64(before))
	if saved := before - after; saved > 0 {
		q.TrimSaved.Add(int64(saved))
	}
}

// RecordAttempt 记一次上游尝试。
func (q *QualityStats) RecordAttempt() {
	q.Attempts.Add(1)
	markQualityDirty()
}

// RecordRequest 记一次客户端请求。
func (q *QualityStats) RecordRequest() {
	q.Requests.Add(1)
	markQualityDirty()
}

// QualitySnapshot 是导出给面板的一份快照。
type QualitySnapshot struct {
	Errors map[string]int64 `json:"errors"`
	// RealErrors 是不含「额度/限流」的错误数 —— 这才是真正值得关注的。
	RealErrors int64 `json:"realErrors"`

	TrimHits   int64   `json:"trimHits"`
	TrimSaved  int64   `json:"trimSaved"`
	TrimBefore int64   `json:"trimBefore"`
	TrimRatio  float64 `json:"trimRatio"` // 省下的占比（0~1）

	Requests    int64   `json:"requests"`
	Attempts    int64   `json:"attempts"`
	AvgAttempts float64 `json:"avgAttempts"` // >1.5 说明换号频繁
}

// Snapshot 取一份当前快照。
func (q *QualityStats) Snapshot() QualitySnapshot {
	out := QualitySnapshot{
		Errors:     map[string]int64{},
		TrimHits:   q.TrimHits.Load(),
		TrimSaved:  q.TrimSaved.Load(),
		TrimBefore: q.TrimBefore.Load(),
		Requests:   q.Requests.Load(),
		Attempts:   q.Attempts.Load(),
	}

	q.errMu.RLock()
	var real int64
	for kind, c := range q.errCnts {
		v := c.Load()
		if v == 0 {
			continue
		}
		out.Errors[kind] = v
		// 额度与限流不是故障，不计入真实错误
		if kind != errQuota && kind != errRateLimit {
			real += v
		}
	}
	q.errMu.RUnlock()
	out.RealErrors = real

	if out.TrimBefore > 0 {
		out.TrimRatio = clampRatio(float64(out.TrimSaved) / float64(out.TrimBefore))
	}
	if out.Requests > 0 {
		out.AvgAttempts = float64(out.Attempts) / float64(out.Requests)
	}
	return out
}

// ResetQuality 清零质量统计。
func (q *QualityStats) Reset() {
	q.errMu.Lock()
	q.errCnts = map[string]*atomic.Int64{}
	q.errMu.Unlock()
	q.TrimHits.Store(0)
	q.TrimSaved.Store(0)
	q.TrimBefore.Store(0)
	q.Attempts.Store(0)
	q.Requests.Store(0)
	q.AccountSwitches.Store(0)
}

// classifyUpstreamError 把一个上游失败归类。
//
// 分类的意义在于判断「该不该管」：
//   - quota / ratelimit：账号没额度或被限流，换号或等窗口，不算 bug
//   - auth：凭据失效，要重新登录
//   - context：上下文超限，要裁剪
//   - 其余：才是需要看的真故障
func classifyUpstreamError(status int, body string, authErr, rateLimit, contextErr bool) string {
	switch {
	case contextErr:
		return errContext
	case authErr:
		return errAuth
	case status == 429:
		if isCreditExhaustedBody(body) {
			return errQuota
		}
		_ = rateLimit
		return errRateLimit
	case status == 401 || status == 403:
		return errAuth
	case status >= 500:
		return errUpstream
	case status == 0:
		return errNetwork
	case status >= 400:
		return errOther
	}
	return ""
}

// isCreditExhaustedBody 判断 429 响应体是不是额度耗尽（而不是窗口限流）。
func isCreditExhaustedBody(body string) bool {
	if body == "" {
		return false
	}
	// 与 provider.IsCreditExhausted 同口径，这里避免跨包引用造成循环依赖。
	return containsAny(body, `"14018"`, `:14018`, `credits_exhausted`, `额度已用尽`)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// handleQuality 暴露请求质量统计。
//
//	GET  /api/quality                    读快照
//	POST /api/quality/reset?             清零
func (s *Server) handleQuality(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":   true,
			"data": quality.Snapshot(),
			"hint": "errors 里 quota/ratelimit 不算故障；realErrors 才是要点关注的。avgAttempts >1.5 说明换号频繁。",
		})
	case http.MethodPost:
		quality.Reset()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已清零"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
	}
}
