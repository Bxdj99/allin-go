package gateway

import (
	"net/http"

	"allin/internal/config"
)

// 换号策略面板接口。
//
//	GET  /api/switch-policy   读当前策略 + 每项说明
//	POST /api/switch-policy   改策略
//
// 设计意图：这个网关有六类会换账号的判断，每类的代价/收益都不同，
// 尤其换号会让上游前缀缓存失效（整段输入从约 10% 计价变全价）。
// 与其替用户决定，不如把权衡摊在面板上让他自己选。
//
// 这里把「优点/缺点」直接随接口返回，面板只管渲染，
// 避免文案散落在前后端两处、改一处忘一处。

type switchOption struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Desc    string `json:"desc"`
	Pros    string `json:"pros"`
	Cons    string `json:"cons"`
	Type    string `json:"type"` // bool / number / enum
	Default any    `json:"default"`
	Current any    `json:"current"`
	Min     any    `json:"min,omitempty"`
	Max     any    `json:"max,omitempty"`
	// Risk 是「改这个有多容易踩坑」，面板用颜色区分。
	Risk string `json:"risk"` // low / mid / high
}

func (s *Server) handleSwitchPolicy(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	pol := cfg.Server.SwitchPolicyOf()

	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"options": switchOptions(pol),
			"note":    "改完即时生效，不用重启。默认值等于「什么都没动」。",
		})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
		return
	}

	var in struct {
		ReserveAmount     *float64 `json:"reserveAmount"`
		ReserveStrict     *bool    `json:"reserveStrict"`
		ProtectCache      *bool    `json:"protectCache"`
		CacheKeepRate     *float64 `json:"cacheKeepRate"`
		CooldownOnError   *bool    `json:"cooldownOnError"`
		LoadBalance       *bool    `json:"loadBalance"`
		MaxAttempts       *int     `json:"maxAttempts"`
		SessionMinCredits *float64 `json:"sessionMinCredits"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
		return
	}

	// 校验：范围都不大，逐项挡掉明显离谱的值。
	if in.ReserveAmount != nil && (*in.ReserveAmount < 0 || *in.ReserveAmount > 1_000_000) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "余额保底需在 0-1000000 之间"})
		return
	}
	if in.CacheKeepRate != nil && (*in.CacheKeepRate < 0 || *in.CacheKeepRate > 1) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缓存保护门槛需在 0-1 之间（0.5 = 50%）"})
		return
	}
	if in.SessionMinCredits != nil && (*in.SessionMinCredits < 0 || *in.SessionMinCredits > 1_000_000) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "新会话余额门槛需在 0-1000000 之间（0=不限制）"})
		return
	}
	if in.MaxAttempts != nil && (*in.MaxAttempts < 1 || *in.MaxAttempts > 10) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "最大尝试账号数需在 1-10 之间"})
		return
	}

	next := cfg.Server.SwitchPolicy
	if in.ReserveAmount != nil {
		v := *in.ReserveAmount
		next.ReserveAmount = &v
		// 同步写回旧字段，避免两套配置打架
		cfg.Server.CreditReserveAmount = &v
	}
	if in.ReserveStrict != nil {
		v := *in.ReserveStrict
		next.ReserveStrict = &v
		cfg.Server.CreditReserveStrict = &v
	}
	if in.ProtectCache != nil {
		v := *in.ProtectCache
		next.ProtectCache = &v
	}
	if in.CacheKeepRate != nil {
		v := *in.CacheKeepRate
		next.CacheKeepRate = &v
	}
	if in.CooldownOnError != nil {
		v := *in.CooldownOnError
		next.CooldownOnError = &v
	}
	if in.LoadBalance != nil {
		v := *in.LoadBalance
		next.LoadBalance = &v
	}
	if in.MaxAttempts != nil {
		v := *in.MaxAttempts
		next.MaxAttempts = &v
	}
	if in.SessionMinCredits != nil {
		v := *in.SessionMinCredits
		next.SessionMinCredits = &v
	}
	cfg.Server.SwitchPolicy = next

	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	// 即时生效
	s.applySwitchPolicy(cfg)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"options": switchOptions(cfg.Server.SwitchPolicyOf()),
		"message": "已保存并生效",
	})
}

// applySwitchPolicy 把策略推给账号池与保底配置。
func (s *Server) applySwitchPolicy(cfg *config.Config) {
	pol := cfg.Server.SwitchPolicyOf()
	s.pool.SetCreditReserve(true, pol.ReserveAmountValue(), pol.ReserveStrictOn())
	s.pool.SetSwitchPolicy(pol.ProtectCacheOn(), pol.CacheKeepRateValue(), pol.CooldownOnErrorOn())
	s.pool.SetSessionCredits(pol.SessionMinCreditsValue())
	s.applyCreditReserve(cfg)
}

// switchOptions 生成面板要展示的选项清单（含优缺点）。
func switchOptions(pol config.SwitchPolicy) []switchOption {
	return []switchOption{
		{
			Key:  "reserveAmount",
			Name: "余额保底（积分）",
			Desc: "账号剩余积分低于这个值就考虑换号",
			Pros: "留一点余量，避免余额刚好不够一次请求导致失败",
			Cons: "调太高会频繁触发换号判断；而换号会让前缀缓存失效，单次成本可能翻 10 倍",
			Type: "number", Default: 10, Current: pol.ReserveAmountValue(),
			Min: 0, Max: 1000000, Risk: "mid",
		},
		{
			Key:  "protectCache",
			Name: "缓存保护（推荐开启）",
			Desc: "命中率高时不因余额保底而换号",
			Pros: "实测能把 54.9 万 token 的请求从全价降到约 5.5 万等效 token",
			Cons: "账号可能被用到接近枯竭；关闭后换号更积极，余额分布更均匀",
			Type: "bool", Default: true, Current: pol.ProtectCacheOn(),
			Risk: "low",
		},
		{
			Key:  "cacheKeepRate",
			Name: "缓存保护门槛（命中率）",
			Desc: "命中率达到这个比例就不换号；0.5 = 50%",
			Pros: "调高更倾向「留住缓存」，适合长会话",
			Cons: "调太高（如 0.9）会让余额不足的账号继续被用，可能触发额度报错",
			Type: "number", Default: 0.5, Current: pol.CacheKeepRateValue(),
			Min: 0, Max: 1, Risk: "mid",
		},
		{
			Key:  "reserveStrict",
			Name: "严格模式（压过缓存保护）",
			Desc: "余额跌破保底就强制换号，忽略缓存保护；全部账号都不够时直接拒绝",
			Pros: "开启后在「保余额」和「保缓存」之间明确选前者；实测你设的 5-10 分门槛会强制换到厚号",
			Cons: "换号必然丢一次缓存；全部低于保底时会 503，看起来像故障",
			Type: "bool", Default: false, Current: pol.ReserveStrictOn(),
			Risk: "mid",
		},
		{
			Key:  "cooldownOnError",
			Name: "出错冷却",
			Desc: "账号出错后暂时不再派活",
			Pros: "死号/限流号会被自动跳过，不会反复撞墙",
			Cons: "单账号场景下，一次抖动就会让服务短暂不可用",
			Type: "bool", Default: true, Current: pol.CooldownOnErrorOn(),
			Risk: "mid",
		},
		{
			Key:  "loadBalance",
			Name: "负载均衡",
			Desc: "按近期 token 消耗把请求摊到不同账号",
			Pros: "多账号时避免某个号被重活拖垮，余额分布更均匀",
			Cons: "会打破会话绑定；每切一次号就丢一次缓存，长会话反而更贵",
			Type: "bool", Default: true, Current: pol.LoadBalanceOn(),
			Risk: "high",
		},
		{
			Key:  "sessionMinCredits",
			Name: "新会话余额门槛",
			Desc: "新开的会话优先绑余额达到这个值的账号；0 = 不限制",
			Pros: "会话开头就绑在余额厚的号上，不容易中途换号，缓存更稳",
			Cons: "门槛设太高（如 99）会把低余额账号全部排除，一旦厚号耗尽可能无号可用",
			Type: "number", Default: 0, Current: pol.SessionMinCreditsValue(),
			Min: 0, Max: 1000000, Risk: "mid",
		},
		{
			Key:  "maxAttempts",
			Name: "最大尝试账号数",
			Desc: "一个请求最多换几个账号",
			Pros: "调大提高成功率，适合账号多但不稳的情况",
			Cons: "每次重试都是一次完整上游请求，失败时白烧额度；调小则失败更快",
			Type: "number", Default: 3, Current: pol.MaxAttemptsValue(),
			Min: 1, Max: 10, Risk: "mid",
		},
	}
}
