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

	// 积分快照：由后台刷新任务灌入，Pick 只读，绝不在此触发网络请求。
	creditsAt time.Time // 快照时间，零值表示从未刷新过
	remaining float64   // 当前可用余额
	resetAt   time.Time // 最近一次额度重置/到期时间，零值表示未知
}

// CreditsFloor 是「余额还够用」的下限。低于它的账号不会被优先派活，
// 否则会选出只剩零点几分的号，请求立刻因额度不足失败、白跑一次换号重试。
const CreditsFloor = 1.0

// creditsStale 是积分快照的保鲜期。超过这个时间的快照不参与排序，
// 避免用几小时前的旧数据误导选号。
const creditsStale = 30 * time.Minute

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
//
// 排序优先级：
//  1. 跳过冷却中的账号（全部冷却时退化为「最早恢复」）
//  2. 余额充足（>= CreditsFloor 且有新鲜积分快照）的账号优先于余额不足/未知的
//  3. 同为余额充足时，按 least-used 轮转 —— 让多个号分摊负载
//  4. 只在「用得一样多」的账号之间，才用到期时间打破平局（先到期先上）
//
// 第 3 步先于第 4 步是刻意的：如果让「最早到期」无条件压倒轮转，
// 只要那个号余额充足就会被一直选中，其余账号永远闲置，
// 等于退化成单账号。「轮流用」和「优先用快过期的」要同时成立，
// 所以到期时间只用来决定同负载下的先后，不用来垄断选号。
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

	// rank 返回 true 表示 a 优于 b。
	// 比较顺序：余额充足性 → 成功次数 → 到期紧急度 → 最久未用。
	rank := func(a, b *config.Account, sa, sb *acctState) bool {
		aReady, bReady := creditsReady(sa, now), creditsReady(sb, now)
		if aReady != bReady {
			return aReady
		}
		// 先比成功次数，保证轮转：用得少的先上
		var as, bs int64
		if sa != nil {
			as = sa.successes
		}
		if sb != nil {
			bs = sb.successes
		}
		if as != bs {
			return as < bs
		}
		// 用得一样多，再让快过期的优先，把要作废的积分先用掉
		if aReady && bReady {
			ar, br := creditReset(sa), creditReset(sb)
			switch {
			case ar.IsZero() && !br.IsZero():
				return false // 到期未知的排后面
			case !ar.IsZero() && br.IsZero():
				return true
			case !ar.IsZero() && !br.IsZero() && !ar.Equal(br):
				return ar.Before(br)
			}
		}
		// 最后比谁更久没用过
		var al, bl time.Time
		if sa != nil {
			al = sa.lastUsed
		}
		if sb != nil {
			bl = sb.lastUsed
		}
		return al.Before(bl)
	}

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
		if rank(a, best, st, bestState) {
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

// creditsReady 判断账号的积分快照是否可用参与排序：
// 必须有新鲜快照，且余额不低于下限。没有快照时不参与积分排序，
// 退回到原有的 least-used 行为，避免因为查不到积分就改变选号结果。
func creditsReady(st *acctState, now time.Time) bool {
	if st == nil || st.creditsAt.IsZero() {
		return false
	}
	if now.Sub(st.creditsAt) > creditsStale {
		return false
	}
	return st.remaining >= CreditsFloor
}

// creditReset 取账号的额度重置时间，未知返回零值。
func creditReset(st *acctState) time.Time {
	if st == nil {
		return time.Time{}
	}
	return st.resetAt
}

// SetCredits 写入一个账号的积分快照。remaining 是当前可用余额，
// resetAt 是最近一次额度重置/到期时间。由后台刷新任务调用。
func (p *Pool) SetCredits(pid, aid string, remaining float64, resetAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[key(pid, aid)]
	if st == nil {
		st = &acctState{}
		p.state[key(pid, aid)] = st
	}
	st.remaining = remaining
	st.resetAt = resetAt
	st.creditsAt = time.Now()
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
		if !st.creditsAt.IsZero() {
			row["creditsRemaining"] = st.remaining
			row["creditsAgeSec"] = int64(now.Sub(st.creditsAt).Seconds())
			row["creditsReady"] = creditsReady(st, now)
			if !st.resetAt.IsZero() {
				row["creditsResetAt"] = st.resetAt.Format("2006-01-02 15:04")
			}
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
