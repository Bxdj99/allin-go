package provider

import (
	"sync"
	"sync/atomic"
	"time"

	"allin/internal/config"
)

// 账号健康状态。上游 401/403 视为凭据失效，429/5xx 视为临时故障，两者冷却时长不同。
const (
	coolAuth      = 30 * time.Minute // 凭据失效：短时间内别再用
	coolRateLimit = 2 * time.Minute  // 限流
	coolServer    = 1 * time.Minute  // 上游 5xx
	coolNetwork   = 30 * time.Second // 网络层错误
)

// acctState 是单个账号的运行时状态（不落盘）。
type acctState struct {
	failures    int
	coolUntil   time.Time
	lastUsed    time.Time
	lastFailure string
	successes   int64
}

// Pool 管理所有供应商的账号选择与健康状态。
//
// 选择策略：
//  1. 跳过冷却中的账号
//  2. 优先用得最少的（least-used），避免把所有请求压在一个号上
//  3. 全部冷却中时，挑冷却最早结束的那个（总比直接失败强）
type Pool struct {
	mu    sync.Mutex
	state map[string]*acctState // key: providerID + "/" + accountID
}

// NewPool 构造账号池。
func NewPool() *Pool {
	return &Pool{state: make(map[string]*acctState)}
}

func key(pid, aid string) string { return pid + "/" + aid }

// Pick 选一个可用账号。exclude 里的账号会被跳过（用于重试时换号）。
func (p *Pool) Pick(prov *config.Provider, exclude map[string]bool) *config.Account {
	if prov == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var best *config.Account
	var bestState *acctState
	var fallback *config.Account
	var fallbackUntil time.Time

	for _, a := range prov.Accounts {
		if a == nil || !a.IsEnabled() || a.Key() == "" {
			continue
		}
		if exclude != nil && exclude[a.ID] {
			continue
		}
		st := p.state[key(prov.ID, a.ID)]
		if st != nil && now.Before(st.coolUntil) {
			// 冷却中，记下最早恢复的作为兜底
			if fallback == nil || st.coolUntil.Before(fallbackUntil) {
				fallback = a
				fallbackUntil = st.coolUntil
			}
			continue
		}
		if best == nil {
			best, bestState = a, st
			continue
		}
		// 比「成功次数少的优先」，再比「更久没用过的优先」
		bestWins := 0
		var bestSucc, curSucc int64
		if bestState != nil {
			bestSucc = bestState.successes
		}
		if st != nil {
			curSucc = st.successes
		}
		switch {
		case curSucc < bestSucc:
			bestWins = 1
		case curSucc > bestSucc:
			bestWins = -1
		default:
			var bestLast, curLast time.Time
			if bestState != nil {
				bestLast = bestState.lastUsed
			}
			if st != nil {
				curLast = st.lastUsed
			}
			if curLast.Before(bestLast) {
				bestWins = 1
			}
		}
		if bestWins > 0 {
			best, bestState = a, st
		}
	}

	if best == nil {
		best = fallback
	}
	if best != nil {
		st := p.state[key(prov.ID, best.ID)]
		if st == nil {
			st = &acctState{}
			p.state[key(prov.ID, best.ID)] = st
		}
		st.lastUsed = now
	}
	return best
}

// Success 记录一次成功。
func (p *Pool) Success(pid, aid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[key(pid, aid)]
	if st == nil {
		st = &acctState{}
		p.state[key(pid, aid)] = st
	}
	st.failures = 0
	st.coolUntil = time.Time{}
	st.lastFailure = ""
	st.successes++
	st.lastUsed = time.Now()
}

// Fail 记录一次失败，并按状态码决定冷却时长。
func (p *Pool) Fail(pid, aid string, status int, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[key(pid, aid)]
	if st == nil {
		st = &acctState{}
		p.state[key(pid, aid)] = st
	}
	st.failures++
	st.lastFailure = reason

	var cool time.Duration
	switch {
	case status == 401 || status == 403:
		cool = coolAuth
	case status == 429:
		cool = coolRateLimit
	case status >= 500:
		cool = coolServer
	default:
		cool = coolNetwork
	}
	// 连续失败越多次，冷却越久（上限 8 倍）
	mult := st.failures
	if mult > 8 {
		mult = 8
	}
	st.coolUntil = time.Now().Add(cool * time.Duration(mult))
}

// Reset 清空某个账号的冷却状态。
func (p *Pool) Reset(pid, aid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.state, key(pid, aid))
}

// Snapshot 导出全部账号健康状态，供管理接口展示。
func (p *Pool) Snapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]map[string]any, 0, len(p.state))
	for k, st := range p.state {
		row := map[string]any{
			"key":       k,
			"successes": st.successes,
			"failures":  st.failures,
		}
		if !st.lastUsed.IsZero() {
			row["lastUsedSec"] = int64(now.Sub(st.lastUsed).Seconds())
		}
		if now.Before(st.coolUntil) {
			row["cooling"] = true
			row["coolRemainSec"] = int64(st.coolUntil.Sub(now).Seconds())
		}
		if st.lastFailure != "" {
			row["lastFailure"] = st.lastFailure
		}
		out = append(out, row)
	}
	return out
}

// Stats 是进程级的计数（供状态接口）。
type Stats struct {
	Requests atomic.Int64
	Errors   atomic.Int64
	Retries  atomic.Int64
	Switches atomic.Int64
}
