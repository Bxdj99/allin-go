package provider

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"allin/internal/config"
)

// 账号健康状态。上游 401/403 视为凭据失效，429/5xx 视为临时故障，两者冷却时长不同。
const (
	coolAuth = 30 * time.Minute // 凭据失效：短时间内别再用
	// coolDeadCredential 是「凭据确定失效」的冷却时长。
	// token 过期或账号被封不会自己好，继续每 10 分钟试一次纯属浪费。
	coolDeadCredential = 7 * 24 * time.Hour
	coolRateLimit      = 2 * time.Minute  // 限流
	coolServer         = 1 * time.Minute  // 上游 5xx
	coolNetwork        = 30 * time.Second // 网络层错误
)

// acctState 是单个账号的运行时状态（不落盘）。
type acctState struct {
	failures    int
	coolUntil   time.Time
	lastUsed    time.Time
	lastFailure string
	successes   int64

	// needRelogin 表示凭据已被上游判死（连续多次 401 且本地没有 refresh）。
	// 这种账号不会再被选中，面板上要明确标出来让用户重新登录，
	// 否则用户只会看到「冷却中」，以为是临时限流。
	needRelogin bool

	// 积分快照：由后台刷新任务灌入，Pick 只读，绝不在此触发网络请求。
	creditsAt   time.Time // 快照时间，零值表示从未刷新过
	remaining   float64   // 当前可用余额
	creditTotal float64   // 当前周期总额度，用于算余额比例
	resetAt     time.Time // 最近一次额度重置/到期时间，零值表示未知
}

// CreditsFloor 是「余额还够用」的下限。低于它的账号不会被优先派活，
// 否则会选出只剩零点几分的号，请求立刻因额度不足失败、白跑一次换号重试。
const CreditsFloor = 1.0

// creditsStale 是积分快照的保鲜期。
//
// v1.39.0 从 30 分钟收紧到 2 分钟（对齐上游 AccountRoutingPolicy 的 FRESH_MS=120s）。
// 原因：半小时前的余额等于过期地图 —— 期间某个号可能已经耗尽，
// 却仍被当成「余额充足」选中，白打一次上游并触发 429。
// 线上 451 次 429 全部是额度耗尽，与慢刷新直接相关。
//
// 注意：光改这里没用，creditsRefreshInterval 必须同步调快，
// 否则快照永远「过期」，保底与选号会一起退化成未知状态。
const creditsStale = 2 * time.Minute

// loadTie 是「负载视为持平」的阈值。
// 上游用 0.5 个百分点的余额比例作为分档，这里对 token 负载取一个绝对值：
// 一次普通对话也就几千 token，小于这个数量级的差异不值得改变选号结果。
const loadTie = 2000.0

// cacheKeepMinHitRate 是「值得为缓存而保留账号」的最低命中率。
//
// 取值依据：命中部分的成本约为未命中的 1/10。
// 命中率 h 时的相对成本 ≈ h*0.1 + (1-h)。
//
//	h=90% → 0.19（省 81%）
//	h=50% → 0.55（省 45%）
//	h=30% → 0.73（省 27%）
//
// 取 50% 作为门槛：此时换号至少要多花 45% 的输入成本，
// 而保底想守住的那点余额几乎不可能补回这笔差价。
const cacheKeepMinHitRate = 0.5

// Pool 管理所有供应商的账号选择与健康状态。
//
// 选择策略：
//  1. 跳过冷却中的账号
//  2. 优先用得最少的（least-used），避免把所有请求压在一个号上
//  3. 全部冷却中时，挑冷却最早结束的那个（总比直接失败强）
type Pool struct {
	mu    sync.Mutex
	state map[string]*acctState // key: providerID + "/" + accountID

	// sessions 记录「会话 → 账号」绑定，供会话绑定策略使用。
	sessions *sessionTable

	// reserveMu 保护下面几个保底字段（配置可能热更新）。
	reserveMu     sync.RWMutex
	reserveOn     bool
	reserveAmount float64
	// reserveStrict 为真时，全部账号低于保底值就不派活（返回 nil）。
	// 为假时退化为「挑余额最高的那个用」，避免用户看到「全部不可用」。
	reserveStrict bool

	// load 按「近期真实 token 消耗」给账号打分，用于把重活摊开。
	// 见 scheduler.go 的说明：只按请求次数轮转会轻重大小不分。
	load *UsageScheduler
}

// SetCreditReserve 设置「切号保底」：开启后余额低于 amount 的账号不再派活。
// 由网关在启动时与配置热更新时调用。
func (p *Pool) SetCreditReserve(on bool, amount float64, strict bool) {
	p.reserveMu.Lock()
	defer p.reserveMu.Unlock()
	p.reserveOn = on
	if amount < 0 {
		amount = 0
	}
	p.reserveAmount = amount
	p.reserveStrict = strict
}

// reserveActive 判断当前是否「保底生效」（开启且门槛确实被抬高）。
func (p *Pool) reserveActive() bool {
	p.reserveMu.RLock()
	defer p.reserveMu.RUnlock()
	return p.reserveOn && p.reserveAmount > CreditsFloor
}

// reserveIsStrict 判断严格模式是否开启。
func (p *Pool) reserveIsStrict() bool {
	p.reserveMu.RLock()
	defer p.reserveMu.RUnlock()
	return p.reserveStrict
}

// accountBelowReserve 判断账号是否「不该再派活」。
//
// 两种情况都算「低于保底」：
//  1. 有新鲜快照且余额 < 门槛 —— 明确不够；
//  2. 快照过期或从未拿到（余额未知）—— 判断不了。
//
// 第 2 条是 v1.36.0 的行为变更。旧实现把「未知」当成「够用」，于是
// 积分刷新一断（超过 creditsStale）保底就整体静默失效：
// 余额 0 的号照样被选中，日志和面板都没有任何提示。
// 线上表现就是「设了保底 50，账号耗到 0 还能正常用」。
//
// 「是否真的拦」由调用方结合严格模式判断，见 shouldSwitchAway。
func (p *Pool) accountBelowReserve(st *acctState, now time.Time) bool {
	if st == nil {
		return false
	}
	if p.creditUnknown(st, now) {
		return p.reserveIsStrict()
	}
	return st.remaining < p.creditFloor()
}

// creditUnknown 判断余额快照是否「过期或不存在」。
func (p *Pool) creditUnknown(st *acctState, now time.Time) bool {
	return st == nil || st.creditsAt.IsZero() || now.Sub(st.creditsAt) > creditsStale
}

// shouldSwitchAway 判断选号时是否应该跳过这个账号。
//
// 保底关闭时只挡「余额见底」（CreditsFloor=1）：连 1 分都没有的号派了也白派。
// 保底开启且严格模式时，余额未知同样跳过 —— 宁可不派，也不拿未知余额的号去烧。
// 非严格模式保持旧行为：未知就放行，避免用户看到「全部不可用」。
func (p *Pool) shouldSwitchAway(st *acctState, now time.Time) bool {
	if st == nil {
		return false
	}
	if p.creditUnknown(st, now) {
		// 余额未知。严格模式拦，非严格模式放行。
		return p.reserveActive() && p.reserveIsStrict()
	}
	return st.remaining < p.creditFloor()
}

// creditFloor 返回当前生效的余额下限：
//   - 保底关闭时就是 CreditsFloor（只挡住余额见底的号）
//   - 保底开启时取 CreditsFloor 与 reserveAmount 的较大者
//
// 这样「保底」只是把门槛抬高，不会把原本的「够用」判断弄乱。
func (p *Pool) creditFloor() float64 {
	p.reserveMu.RLock()
	on, amount := p.reserveOn, p.reserveAmount
	p.reserveMu.RUnlock()
	if on && amount > CreditsFloor {
		return amount
	}
	return CreditsFloor
}

// NewPool 构造账号池。
func NewPool() *Pool {
	return &Pool{
		state:    make(map[string]*acctState),
		sessions: newSessionTable(),
		load:     NewUsageScheduler(),
	}
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
	return p.PickForSession(prov, exclude, "")
}

// PickForSession 按供应商的路由策略挑账号。
//
// sessionKey 为空时退化为无绑定的普通选择。
// 返回的账号已经过：冷却过滤 → 余额过滤 → 策略排序。
func (p *Pool) PickForSession(prov *config.Provider, exclude map[string]bool, sessionKey string) *config.Account {
	if prov == nil {
		return nil
	}

	mode := prov.RouteModeOrDefault()
	// 会话绑定：先看有没有现成的绑定，且该账号仍可用
	if mode == config.RouteSticky && sessionKey != "" {
		if aid := p.sessions.Lookup(sessionKey); aid != "" {
			if acct, ok := p.accountUsable(prov, aid, exclude); ok {
				return acct
			}
			// 账号不可用（冷却/余额不足/被排除）就解绑，重新选
			p.sessions.Unbind(sessionKey)
		}
	}

	// sticky 首次选号：直接取「已绑定会话数最少」的可用账号。
	//
	// 不能用 pickRanked：它做完整比较，全新会话的余额/到期/成功次数完全相同，
	// 最终会退化成「遍历到的第一个」，排序结果用不上。
	if mode == config.RouteSticky {
		if acct := p.pickLeastLoaded(prov, exclude); acct != nil {
			p.sessions.Bind(sessionKey, acct.ID)
			log.Printf("[STICKY] key=%.8s -> %s", sessionKey, acct.ID)
			return acct
		}
	}
	acct := p.pickByMode(prov, exclude, mode)
	if acct != nil && mode == config.RouteSticky && sessionKey != "" {
		p.sessions.Bind(sessionKey, acct.ID)
	}
	return acct
}

// accountUsable 检查指定账号现在能不能用（用于校验已有绑定）。
func (p *Pool) accountUsable(prov *config.Provider, aid string, exclude map[string]bool) (*config.Account, bool) {
	if exclude != nil && exclude[aid] {
		return nil, false
	}
	for _, a := range prov.Accounts {
		if a == nil || a.ID != aid {
			continue
		}
		if !a.IsEnabled() || a.Key() == "" {
			return nil, false
		}
		p.mu.Lock()
		st := p.state[key(prov.ID, aid)]
		now := time.Now()
		if st != nil && now.Before(st.coolUntil) {
			p.mu.Unlock()
			return nil, false
		}
		// 余额低于下限就换号，否则接下来必然失败。
		//
		// 这里连带处理「余额未知」：快照过期在严格模式下同样视为不可用。
		// v1.36.0 前只拦「有新鲜快照且确实低于门槛」，快照一过期就静默放行，
		// 保底形同虚设。
		//
		// 会话已绑定的账号也会走到这里：余额跌破门槛时强制解绑换号。
		// 代价是长对话可能中途换号，换号后上游 prompt cache 会失效一次
		// （上下文由网关侧维护，对话本身不会断）。
		if st != nil && p.shouldSwitchAway(st, now) {
			// 缓存优先：命中率高的会话换号代价极大。
			//
			// 实测（同一会话 54.9 万 token）：
			//   留在原账号（余额 47，命中 99.9%）→ 等效约 5.5 万 token
			//   切到新账号（余额 1500，命中 0%） → 实际 54.9 万 token
			// 为保住 47 积分而多花 10 倍 token，明显是亏的。
			rate, known := p.CacheHealth(prov.ID, aid)
			if known && rate >= cacheKeepMinHitRate {
				log.Printf("[RESERVE] %s 余额 %.0f 低于门槛 %.0f，但缓存命中 %.0f%%，继续用（换号会丢缓存）",
					aid, st.remaining, p.creditFloor(), rate*100)
				// 保持可用：不换号，继续沿用。下面走和正常路径一样的收尾。
				if st == nil {
					st = &acctState{}
					p.state[key(prov.ID, aid)] = st
				}
				st.lastUsed = now
				p.mu.Unlock()
				_ = a
				return a, true
			}
			p.mu.Unlock()
			if p.creditUnknown(st, now) {
				log.Printf("[RESERVE] %s 余额快照过期，严格模式下解绑换号", aid)
			} else {
				log.Printf("[RESERVE] %s 余额 %.0f 低于门槛 %.0f，解绑换号", aid, st.remaining, p.creditFloor())
			}
			return nil, false
		}
		if st == nil {
			st = &acctState{}
			p.state[key(prov.ID, aid)] = st
		}
		st.lastUsed = now
		p.mu.Unlock()
		return a, true
	}
	return nil, false
}

// pickByMode 按策略从候选里挑一个。
func (p *Pool) pickByMode(prov *config.Provider, exclude map[string]bool, mode string) *config.Account {
	if mode == config.RouteSticky || mode == config.RouteRotate ||
		mode == config.RouteBalance || mode == config.RouteExpiry {
		return p.pickRanked(prov, exclude, mode)
	}
	return p.pickRanked(prov, exclude, config.RouteSticky)
}

// UnbindSession 在账号失败时解除绑定，下次请求重新选号。
func (p *Pool) UnbindSession(sessionKey string) {
	p.sessions.Unbind(sessionKey)
}

// pickRanked 是核心选择逻辑：过滤冷却与余额 → 按策略排序。
func (p *Pool) pickRanked(prov *config.Provider, exclude map[string]bool, mode string) *config.Account {
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
	// lowFallback 记住「余额不足但其它方面可用」的第一个账号。
	// 非严格模式下如果全都不足，就退回用它 —— 旧行为是「不会让用户看到全部不可用」。
	var lowFallback *config.Account
	var lowFallbackState *acctState

	// creditCoverage：只有当**所有候选**都有新鲜额度时，才允许拿额度来排序。
	//
	// 这是上游 AccountRoutingPolicy.java:107-118 的语义，也是我们曾经的坑：
	// 旧实现逐个账号判 creditsReady，于是在「A 有新鲜快照、B 快照过期」时
	// 直接让 A 压过 B —— 等于把「未知」当成「比 A 少」，
	// 积分刷新一断就会把流量全压到少数几个有数据的号上。
	// 覆盖率不全时统一退回 token/轮转指标，谁也不因「查不到」而吃亏。
	creditCoverage := true
	{
		floor := p.creditFloor()
		checked := 0
		for _, a := range prov.Accounts {
			if a == nil || !a.IsEnabled() || a.Key() == "" {
				continue
			}
			if exclude != nil && exclude[a.ID] {
				continue
			}
			st := p.state[key(prov.ID, a.ID)]
			if st != nil && now.Before(st.coolUntil) {
				continue
			}
			checked++
			if !creditsReady(st, now, floor) {
				creditCoverage = false
				break
			}
		}
		if checked == 0 {
			creditCoverage = false
		}
	}

	// rank 返回 true 表示 a 优于 b。
	// 比较顺序：余额充足性（仅在覆盖率完整时）→ 成功次数 → 到期紧急度 → 最久未用。
	rank := func(a, b *config.Account, sa, sb *acctState) bool {
		floor := p.creditFloor()
		aReady, bReady := creditsReady(sa, now, floor), creditsReady(sb, now, floor)
		// 覆盖率不完整时不比余额充足性：未知不等于不足。
		if creditCoverage && aReady != bReady {
			return aReady
		}
		// 按路由策略决定比较顺序。
		//
		// balance：先比「余额比例」低的，让各号消耗速度趋同。
		// expiry ：先比到期时间近的，把要作废的额度先用掉。
		// rotate ：只比成功次数，纯粹均摊。
		// sticky ：已由 PickForSession 绑定处理，这里的 rank 用于首次选号，
		//          与 expiry 类似（快到期的优先），避免额度浪费。
		switch mode {
		case config.RouteBalance:
			if aReady && bReady {
				ap, bp := creditPct(sa), creditPct(sb)
				if ap != bp {
					return ap < bp
				}
			}
		case config.RouteExpiry, config.RouteSticky:
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
		}

		// 按「近期真实 token 负载」分高低，这是把重活摊开的关键一步。
		//
		// 只按成功次数轮转的问题是轻重大小不分：
		// 线上两个账号，202 次请求吃掉 7443 万输入 token，
		// 1028 次请求吃掉 2.39 亿 —— 次数差 5 倍，消耗却接近。
		// 按 token 加权后，刚跑过重活的号会靠后，请求自然流向更轻的号。
		//
		// 负载差在 LOAD_TIE 以内视为持平，继续走下面的轮转，
		// 免得几 token 的微小差异就完全压过其它策略。
		la := p.load.Load(key(prov.ID, a.ID))
		lb := p.load.Load(key(prov.ID, b.ID))
		if diff := la - lb; diff > loadTie || diff < -loadTie {
			return la < lb
		}

		// 轮转兜底：成功次数少的先上
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
		// 余额明确不够（或严格模式下余额未知）的账号在候选阶段就剔除。
		//
		// 这一步和下面的 rank 分工不同，别合并：
		//   这里判「够不够用」——低于门槛就是不够，是确定事实，与覆盖率无关；
		//   rank 比「谁更富余」——只有全部候选都有新鲜读数时才可比。
		//
		// 旧实现完全没有这一步，保底只靠函数末尾那一道 strict 检查，
		// 于是非严格模式下余额不足的号照样被选中。
		//
		// 只在这个账号**确实有读数且低于门槛**时剔除；
		// 余额未知交给严格模式决定（非严格视为可用，避免「全部不可用」）。
		if st != nil && !p.creditUnknown(st, now) && st.remaining < p.creditFloor() {
			if lowFallback == nil {
				lowFallback, lowFallbackState = a, st
			}
			continue
		}
		if st != nil && p.creditUnknown(st, now) && p.reserveActive() && p.reserveIsStrict() {
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
	// 连冷却兜底都没有，说明候选全被余额过滤掉了。
	// 非严格模式退回余额最低门槛的那个（旧行为：不让用户看到「全部不可用」）；
	// 严格模式保持 nil，由下面统一拒绝。
	if best == nil && !p.reserveIsStrict() {
		best, bestState = lowFallback, lowFallbackState
	}
	// 严格模式：选出来的账号若也低于保底值，就别派活。
	// 这里覆盖两种情况——正常候选全被过滤掉后退到 fallback，
	// 或者 fallback 本身就是个余额见底的号。
	if best != nil && p.reserveActive() && p.reserveIsStrict() {
		if p.accountBelowReserve(p.state[key(prov.ID, best.ID)], now) {
			return nil
		}
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
func creditsReady(st *acctState, now time.Time, floor float64) bool {
	if st == nil || st.creditsAt.IsZero() {
		return false
	}
	if now.Sub(st.creditsAt) > creditsStale {
		return false
	}
	return st.remaining >= floor
}

// creditPct 返回账号剩余额度比例（0~1）。无数据时返回 2（排后面）。
func creditPct(st *acctState) float64 {
	if st == nil || st.creditTotal <= 0 {
		return 2
	}
	return st.remaining / st.creditTotal
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
	p.SetCreditsFull(pid, aid, remaining, 0, resetAt)
}

// SetCreditsFull 额外记录周期总额度，供「用量均衡」策略计算比例。
func (p *Pool) SetCreditsFull(pid, aid string, remaining, total float64, resetAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[key(pid, aid)]
	if st == nil {
		st = &acctState{}
		p.state[key(pid, aid)] = st
	}
	st.remaining = remaining
	st.creditTotal = total
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
	st.needRelogin = false
	st.successes++
	st.lastUsed = time.Now()
}

// Fail 记录一次失败，并按状态码决定冷却时长。
//
// 只知道状态码时用这个；如果手上有 429 的响应体，
// 请用 FailWithBody —— 它能区分「额度用尽」与「窗口限流」。
func (p *Pool) Fail(pid, aid string, status int, reason string) {
	p.fail(pid, aid, status, reason, "")
}

// FailWithBody 与 Fail 相同，但会解析 429 响应体来决定冷却时长。
//
// 为什么要区分：实测 451 次 429 全部是 code 14018「额度已用尽」，
// 那是不会自愈的状态。按限流冷却 2 分钟的话，
// 每 2 分钟就会拿一个没额度的号去打一次上游，纯浪费。
func (p *Pool) FailWithBody(pid, aid string, status int, reason, errBody string) {
	p.fail(pid, aid, status, reason, errBody)
}

func (p *Pool) fail(pid, aid string, status int, reason, errBody string) {
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
		now := time.Now()
		switch {
		case IsCreditExhausted(errBody):
			// 额度用尽：不会自己恢复。用冷却凭据的时长，
			// 等用户充值或额度周期重置后再试。
			st.coolUntil = now.Add(coolDeadCredential)
			st.lastFailure = "额度已用尽"
			return
		default:
			// 真正的窗口限流：冷却到上游给出的重置时刻。
			if reset := QuotaWindowResetAt(errBody, now); !reset.IsZero() {
				st.coolUntil = reset
				st.lastFailure = "频率限制，等待窗口重置"
				return
			}
		}
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
	// 凭据失效不会自愈：401/403 连续 3 次就按「长期失效」处理，冷却 7 天。
	//
	// 为什么不是首次就判死：单个 401 也可能来自瞬时故障，
	// 一次就冻 7 天代价太大。连续 3 次才是足够可靠的信号。
	//
	// 积分刷新那条路径另有一套更快的判定（见 gateway.credits_refresh）：
	// 它 90 秒一轮，等 3 次会白刷 6 分钟，所以在那里用 MarkNeedRelogin 直接打标。
	if (status == 401 || status == 403) && st.failures >= 3 {
		st.coolUntil = time.Now().Add(coolDeadCredential)
		st.needRelogin = true
		return
	}
	st.coolUntil = time.Now().Add(cool * time.Duration(mult))
}

// Reset 清空某个账号的冷却状态。
// MarkNeedRelogin 供检测接口在确认账号失效时打标。
func (p *Pool) MarkNeedRelogin(pid, aid string, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[key(pid, aid)]
	if st == nil {
		st = &acctState{}
		p.state[key(pid, aid)] = st
	}
	st.needRelogin = on
	if on {
		st.coolUntil = time.Now().Add(coolDeadCredential)
	}
}

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
		if st.needRelogin {
			row["needRelogin"] = true
		}
		if !st.creditsAt.IsZero() {
			row["creditsRemaining"] = st.remaining
			row["creditsAgeSec"] = int64(now.Sub(st.creditsAt).Seconds())
			row["creditsReady"] = creditsReady(st, now, p.creditFloor())
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

// pickLeastLoaded 取「已绑定会话数最少」的可用账号，用于 sticky 首次选号。
// 完全跳过冷却中、被排除、余额不足的账号。
func (p *Pool) pickLeastLoaded(prov *config.Provider, exclude map[string]bool) *config.Account {
	if prov == nil {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	var best *config.Account
	bestLoad := -1
	var fallback *config.Account
	for _, a := range prov.Accounts {
		if a == nil || !a.IsEnabled() || a.Key() == "" {
			continue
		}
		if exclude != nil && exclude[a.ID] {
			continue
		}
		st := p.state[key(prov.ID, a.ID)]
		if st != nil && now.Before(st.coolUntil) {
			continue
		}
		if p.shouldSwitchAway(st, now) {
			continue
		}
		if fallback == nil {
			fallback = a
		}
		load := p.sessions.loadOf(a.ID)
		if bestLoad < 0 || load < bestLoad {
			best, bestLoad = a, load
		}
	}
	// 严格模式：所有账号都低于保底值时，一个都不给。
	// 注意 fallback 里记的是「第一个可用账号」，与余额无关，
	// 所以这里必须显式判断，不能指望 fallback 为 nil。
	if best == nil && p.reserveActive() && p.reserveIsStrict() {
		if fallback != nil && p.accountBelowReserve(p.state[key(prov.ID, fallback.ID)], now) {
			p.mu.Unlock()
			return nil
		}
	}
	if best != nil {
		st := p.state[key(prov.ID, best.ID)]
		if st == nil {
			st = &acctState{}
			p.state[key(prov.ID, best.ID)] = st
		}
		st.lastUsed = now
	}
	p.mu.Unlock()
	if best != nil {
		return best
	}
	return fallback
}

// NeedRelogin 返回某账号是否已被标记为「需要重新登录」。
func (p *Pool) NeedRelogin(pid, aid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[key(pid, aid)]
	return st != nil && st.needRelogin
}

// LastRemaining 返回该账号上一次快照里的余额。
// ok=false 表示还没有过快照，调用方不该用它算差值。
func (p *Pool) LastRemaining(pid, aid string) (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[key(pid, aid)]
	if st == nil || st.creditsAt.IsZero() {
		return 0, false
	}
	return st.remaining, true
}

// NeedsRelogin 判断账号是否已被标记为「需要重新登录」。
//
// 用途：积分刷新这类周期性任务据此跳过死号，
// 避免每轮都对着一个不可能成功的账号打一次请求（线上曾累积 882 条 401）。
// 用户重新登录后由 MarkNeedRelogin(false) 或 Success() 清除。
func (p *Pool) NeedsRelogin(pid, aid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[key(pid, aid)]
	return st != nil && st.needRelogin
}

// ReserveLoad 为某个账号占一个在途名额，返回释放函数。
//
// 用途：请求真正发出前后各调用一次，让调度器知道「这个号正在跑」。
// 不占用的话，并发首请求会全压到同一个「此刻最轻」的号上。
//
// 释放函数幂等，且必须调用 —— 否则名额泄漏，该号会被永久低估价值。
func (p *Pool) ReserveLoad(pid, aid string) func(tokens float64) {
	ref := key(pid, aid)
	_, release, ok := p.load.PickAndReserve([]string{ref})
	if !ok {
		return func(float64) {}
	}
	return release
}

// ChargeLoad 直接给账号记一次消耗（没有走 ReserveLoad 的场景）。
func (p *Pool) ChargeLoad(pid, aid string, tokens float64) {
	p.load.Charge(key(pid, aid), tokens)
}

// LoadOf 返回账号当前负载，供诊断与面板展示。
func (p *Pool) LoadOf(pid, aid string) float64 {
	return p.load.Load(key(pid, aid))
}

// RecordCacheRate 由网关在记账时调用，记录账号的缓存命中情况。
func (p *Pool) RecordCacheRate(pid, aid string, cacheRead, prompt int64, known bool) {
	recordCacheRate(pid, aid, cacheRead, prompt, known)
}
