package gateway

import (
	"sort"
	"sync"
	"time"
)

// ttftSample 是一次请求的首字延迟采样。
type ttftSample struct {
	At         int64  `json:"at"`         // Unix 秒，采样时刻
	Model      string `json:"model"`      // 客户端请求的模型名
	Account    string `json:"account"`    // 实际用的账号 id
	TotalMs    int64  `json:"totalMs"`    // 从收到请求到上游返回响应头
	UpstreamMs int64  `json:"upstreamMs"` // 这一轮上游往返
	Attempts   int    `json:"attempts"`   // 尝试了几次（>1 说明换号了）
	Status     int    `json:"status"`     // 上游状态码
	OverheadMs int64  `json:"overheadMs"` // TotalMs - UpstreamMs，网关自身开销
}

// ttftBuffer 是容量固定的环形缓冲，只保留最近 N 次请求。
//
// 为什么要留在内存里而不是解析日志：面板要能直接给用户看，
// 而反馈「调用慢」的人几乎不会去翻 gateway.log。
const ttftCapacity = 200

type ttftBuffer struct {
	mu   sync.Mutex
	buf  []ttftSample // 环形，最多 ttftCapacity 条
	next int
	full bool
}

func newTTFTBuffer() *ttftBuffer {
	return &ttftBuffer{buf: make([]ttftSample, ttftCapacity)}
}

// Add 记一次采样。
func (b *ttftBuffer) Add(s ttftSample) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.At == 0 {
		s.At = time.Now().Unix()
	}
	b.buf[b.next] = s
	b.next++
	if b.next >= ttftCapacity {
		b.next = 0
		b.full = true
	}
}

// Recent 按时间从旧到新返回最近 n 条。n<=0 表示全部。
func (b *ttftBuffer) Recent(n int) []ttftSample {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	count := b.next
	if b.full {
		count = ttftCapacity
	}
	if n > 0 && n < count {
		count = n
	}
	out := make([]ttftSample, 0, count)
	// 从最旧的一条开始取
	start := b.next - count
	if b.full {
		start = b.next
	}
	for i := 0; i < count; i++ {
		idx := (start + i) % ttftCapacity
		out = append(out, b.buf[idx])
	}
	return out
}

// TTFTSummary 是给面板看的一组统计量。
type TTFTSummary struct {
	Count      int     `json:"count"`      // 样本数
	P50Ms      int64   `json:"p50Ms"`      // 中位数
	P90Ms      int64   `json:"p90Ms"`      // P90
	MaxMs      int64   `json:"maxMs"`      // 最大
	AvgMs      int64   `json:"avgMs"`      // 平均
	AvgUpMs    int64   `json:"avgUpstreamMs"` // 上游平均
	AvgOverMs  int64   `json:"avgOverheadMs"` // 网关自身上平均
	Retried    int     `json:"retried"`    // 发生过换号的次数
	SlowCount  int     `json:"slowCount"`  // 超过 3 秒的次数
	Samples    []ttftSample `json:"samples"` // 明细，供图表
}

// Summary 汇总最近 n 条采样。
func (b *ttftBuffer) Summary(n int) TTFTSummary {
	samples := b.Recent(n)
	sum := TTFTSummary{Count: len(samples), Samples: samples}
	if len(samples) == 0 {
		return sum
	}

	totals := make([]int64, 0, len(samples))
	var sumTotal, sumUp, sumOver int64
	for _, s := range samples {
		totals = append(totals, s.TotalMs)
		sumTotal += s.TotalMs
		sumUp += s.UpstreamMs
		sumOver += s.OverheadMs
		if s.Attempts > 1 {
			sum.Retried++
		}
		if s.TotalMs > 3000 {
			sum.SlowCount++
		}
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })

	cnt := int64(len(totals))
	sum.AvgMs = sumTotal / cnt
	sum.AvgUpMs = sumUp / cnt
	sum.AvgOverMs = sumOver / cnt
	sum.MaxMs = totals[len(totals)-1]
	sum.P50Ms = percentile(totals, 50)
	sum.P90Ms = percentile(totals, 90)
	return sum
}

// percentile 取百分位（最近秩法，输入须已升序）。
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	// 最近秩：ceil(p/100 * n)，再转 0 基
	idx := (p*len(sorted) + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

// Clear 清空全部采样（关闭日志开关时调用）。
func (b *ttftBuffer) Clear() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = make([]ttftSample, ttftCapacity)
	b.next = 0
	b.full = false
}
