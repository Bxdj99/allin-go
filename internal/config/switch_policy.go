package config

// 换号策略配置。
//
// 背景：这个网关有六类会触发「换账号」的判断，每类的代价和收益都不同 ——
// 尤其换号会让上游前缀缓存失效（整段输入从约 10% 计价变全价），
// 所以「什么时候该换」不能一刀切，得让用户自己权衡。
//
// 这里只做开关与阈值，不改任何默认行为（默认全开，与改动前一致）。

// SwitchPolicy 是换号策略集合。
type SwitchPolicy struct {
	// --- 1. 余额保底 ---
	// ReserveAmount 余额低于这个值就考虑换号。0 表示用默认 10。
	ReserveAmount *float64 `json:"reserveAmount,omitempty"`
	// ReserveStrict 全部账号低于保底时是否直接拒绝派活。
	ReserveStrict *bool `json:"reserveStrict,omitempty"`
	// ProtectCache 命中率高时不因保底换号（缓存保护）。默认开启。
	ProtectCache *bool `json:"protectCache,omitempty"`
	// CacheKeepRate 缓存保护的门槛：命中率达到它就不换号。0 用默认 0.5。
	CacheKeepRate *float64 `json:"cacheKeepRate,omitempty"`

	// --- 2. 冷却 ---
	// CooldownOnError 出错后是否让该账号进入冷却。默认开启。
	CooldownOnError *bool `json:"cooldownOnError,omitempty"`

	// --- 3. 负载均衡 ---
	// LoadBalance 是否按近期 token 负载把请求摊到不同账号。默认开启。
	// 关掉后完全跟随会话绑定：同一对话几乎不会再换号，缓存最稳。
	LoadBalance *bool `json:"loadBalance,omitempty"`

	// --- 4. 重试上限 ---
	// MaxAttempts 一个请求最多尝试几个账号。0 用默认 3。
	MaxAttempts *int `json:"maxAttempts,omitempty"`
}

// ReserveAmountValue 返回余额保底值；未配置或 0 时用 10。
//
// 默认从 20 改成 10 的理由：实测账号剩 47 分就触发换号，
// 而那次换号让 54.9 万 token 从缓存价变全价，多花约 10 倍。
// 保留一点点余量（10）防止「余额刚好不够一次请求」即可，
// 不必为了守住大额底仓而频繁牺牲缓存。
func (p SwitchPolicy) ReserveAmountValue() float64 {
	if p.ReserveAmount == nil || *p.ReserveAmount <= 0 {
		return 10
	}
	return *p.ReserveAmount
}

// ReserveStrictOn 严格模式，默认关闭。
func (p SwitchPolicy) ReserveStrictOn() bool {
	return p.ReserveStrict != nil && *p.ReserveStrict
}

// ProtectCacheOn 缓存保护，默认开启。
func (p SwitchPolicy) ProtectCacheOn() bool {
	return p.ProtectCache == nil || *p.ProtectCache
}

// CacheKeepRateValue 缓存保护门槛，默认 0.5。
func (p SwitchPolicy) CacheKeepRateValue() float64 {
	if p.CacheKeepRate == nil || *p.CacheKeepRate <= 0 || *p.CacheKeepRate > 1 {
		return 0.5
	}
	return *p.CacheKeepRate
}

// CooldownOnErrorOn 出错冷却，默认开启。
func (p SwitchPolicy) CooldownOnErrorOn() bool {
	return p.CooldownOnError == nil || *p.CooldownOnError
}

// LoadBalanceOn 负载均衡，默认开启。
func (p SwitchPolicy) LoadBalanceOn() bool {
	return p.LoadBalance == nil || *p.LoadBalance
}

// MaxAttemptsValue 最大尝试账号数，默认 3。
func (p SwitchPolicy) MaxAttemptsValue() int {
	if p.MaxAttempts == nil || *p.MaxAttempts <= 0 {
		return 3
	}
	if *p.MaxAttempts > 10 {
		return 10
	}
	return *p.MaxAttempts
}

// SwitchPolicyOf 从 Server 读取策略，并对旧字段做兼容。
//
// 老的 creditReserveAmount / creditReserveStrict 仍然生效：
// 用户在旧面板设过值，不能因为换成新结构就丢掉。
func (s *Server) SwitchPolicyOf() SwitchPolicy {
	p := s.SwitchPolicy
	if p.ReserveAmount == nil && s.CreditReserveAmount != nil {
		p.ReserveAmount = s.CreditReserveAmount
	}
	if p.ReserveStrict == nil && s.CreditReserveStrict != nil {
		p.ReserveStrict = s.CreditReserveStrict
	}
	return p
}
