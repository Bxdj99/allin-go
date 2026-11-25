package provider

import (
	"math"
	"sync"
	"time"
)

// 近期 token 负载调度器。
//
// 目的：把请求按「真实消耗」而不是「次数」摊到各账号上。
//
// 为什么需要它：线上实测两个账号 ——
//
//	202 次请求 → 7443 万输入 token
//	1028 次请求 → 2.39 亿输入 token
//
// 请求数差 5 倍，消耗却差不多。原因是旧实现只按「成功次数」轮转，
// 轻重不分：多次小请求反复唤醒长上下文，比少次重请求更费积分。
//
// 机制（对齐上游 RecentUsageScheduler.java）：
//   - score 累加每次请求的真实 token 数（输入+输出，含缓存读写）
//   - score 按 1 小时半衰期指数衰减，旧账自然遗忘，不会把某个号永久打进冷宫
//   - 加上「在途预留」，避免并发首请求全压到同一个号上
//   - 选 score+预留 最小的账号
//
// 进程内状态，重启即清零。不承诺跨重启均摊。

const (
	// loadHalfLife 是分数衰减的半衰期。
	// 1 小时：既能让刚用过重活的号暂时靠后，又不会让它被长期压制。
	loadHalfLife = time.Hour

	// inFlightReservation 是每个在途请求的预留权重。
	// 上游取 1024；这里沿用同一数量级 ——
	// 目的是让并发场景下的负载判断先粗后细，而不是靠真实 token 猜。
	inFlightReservation = 1024.0

	// schedulerLimit 是跟踪条目上限。超出时优先淘汰最久未动的空闲条目。
	schedulerLimit = 4096
)

// loadEntry 是一个账号的负载记录。
type loadEntry struct {
	score    float64
	at       time.Time // score 的记账时刻（用于衰减）
	inFlight int

	touched uint64 // 最近一次触碰序号，用于淘汰
}

// UsageScheduler 按近期 token 负载调度。
type UsageScheduler struct {
	mu      sync.Mutex
	entries map[string]*loadEntry
	seq     uint64
	now     func() time.Time // 便于测试注入
}

// NewUsageScheduler 构造调度器。
func NewUsageScheduler() *UsageScheduler {
	return &UsageScheduler{entries: make(map[string]*loadEntry), now: time.Now}
}

// decayLocked 把分数按半衰期衰减到 now。调用方须持锁。
func (s *UsageScheduler) decayLocked(e *loadEntry, now time.Time) {
	if now.After(e.at) {
		elapsed := now.Sub(e.at)
		e.score *= powHalf(elapsed)
		e.at = now
	}
}

// powHalf 返回 0.5^(elapsed/halfLife)。
func powHalf(elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(elapsed)/float64(loadHalfLife))
}

// ensureLocked 取或建条目。调用方须持锁。
func (s *UsageScheduler) ensureLocked(ref string, now time.Time) *loadEntry {
	if e := s.entries[ref]; e != nil {
		return e
	}
	if len(s.entries) >= schedulerLimit {
		var oldestRef string
		var oldest uint64 = ^uint64(0)
		found := false
		for r, e := range s.entries {
			if e.inFlight == 0 && e.touched < oldest {
				oldest, oldestRef, found = e.touched, r, true
			}
		}
		if found {
			delete(s.entries, oldestRef)
		} else {
			// 全部在途，不能淘汰 —— 返回 nil 让调用方退回原有排序。
			return nil
		}
	}
	e := &loadEntry{at: now}
	s.entries[ref] = e
	return e
}

// Load 返回某个账号当前的负载（分数 + 在途预留）。未知账号返回 0。
func (s *UsageScheduler) Load(ref string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[ref]
	if e == nil {
		return 0
	}
	s.decayLocked(e, s.now())
	return e.score + float64(e.inFlight)*inFlightReservation
}

// Score 只返回分数部分（不含在途），供诊断用。
func (s *UsageScheduler) Score(ref string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[ref]
	if e == nil {
		return 0
	}
	s.decayLocked(e, s.now())
	return e.score
}

// InFlight 返回在途数。
func (s *UsageScheduler) InFlight(ref string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[ref]; e != nil {
		return e.inFlight
	}
	return 0
}

// PickAndReserve 在候选里挑负载最低的一个并占用一个在途名额。
//
// 返回的 release 必须在请求结束后调用一次（成功传 token 数，失败传 0），
// 否则在途名额会一直占着，导致该账号被永久低估价值。
//
// 候选顺序由调用方给定，相同负载时按给定顺序取先者（保持稳定）。
func (s *UsageScheduler) PickAndReserve(refs []string) (picked string, release func(tokens float64), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	seen := make(map[string]bool, len(refs))
	best := ""
	bestLoad := 0.0
	for _, ref := range refs {
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		e := s.entries[ref]
		var load float64
		if e != nil {
			s.decayLocked(e, now)
			load = e.score + float64(e.inFlight)*inFlightReservation
		}
		if best == "" || load < bestLoad {
			best, bestLoad = ref, load
		}
	}
	if best == "" {
		return "", nil, false
	}

	e := s.ensureLocked(best, now)
	if e == nil {
		return "", nil, false
	}
	e.inFlight++
	s.seq++
	e.touched = s.seq

	finished := false
	release = func(tokens float64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if finished {
			return
		}
		finished = true
		s.settleLocked(e, tokens)
	}
	return best, release, true
}

// settleLocked 结账：先衰减再累加，保证新消耗不被误衰减。
func (s *UsageScheduler) settleLocked(e *loadEntry, tokens float64) {
	now := s.now()
	s.decayLocked(e, now)
	if e.inFlight > 0 {
		e.inFlight--
	}
	if tokens > 0 {
		e.score += tokens
	}
	s.seq++
	e.touched = s.seq
}

// Charge 直接给某个账号加分（没有走 PickAndReserve 的路径用）。
func (s *UsageScheduler) Charge(ref string, tokens float64) {
	if ref == "" || tokens <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e := s.ensureLocked(ref, now)
	if e == nil {
		return
	}
	s.decayLocked(e, now)
	e.score += tokens
	s.seq++
	e.touched = s.seq
}

// Reset 清空全部负载记录（配置变更或需要重新开始时用）。
func (s *UsageScheduler) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]*loadEntry)
	s.seq = 0
}
