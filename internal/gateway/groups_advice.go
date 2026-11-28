package gateway

import (
	"fmt"
	"strings"

	"allin/internal/config"
	"allin/internal/provider"
)

// 策略的积分（token）消耗建议。
//
// 这块的结论不是拍脑袋，是项目里已有的实测：
//
//	换号会让上游前缀缓存失效 —— 整段输入从约 10% 计价变全价，
//	实测同一会话 54.9 万 token，换不换号差 10 倍。
//
// 所以决定消耗的是【换不换账号】，不是换不换供应商。
// 一个路由组里，每个目标背后都是一个独立的账号/凭据：
// 轮到别的目标 = 换了账号 = 缓存前缀作废 = 整个上下文按全价重算。
//
// 按这个顺序排影响：
//  1. 每轮都换账号（轮询均摊）→ 缓存基本不可能复用，最烧积分
//  2. 换到跨供应商的目标 → 同样换号，且上游都不同，一样全价重算
//  3. 只在失败时换（顺序主备）→ 常态不换号，缓存能长期命中，最省
//  4. 固定一个（手动指定）→ 最稳，但额度压在单号上，用完那一下照样全价重建

// StrategyAdvice 是给用户看的一条策略建议。
type StrategyAdvice struct {
	Strategy string `json:"strategy"`
	Token    string `json:"token"`   // 一句话结论：省 / 一般 / 费
	Level    string `json:"level"`   // low / mid / high（面板上色用）
	Detail   string `json:"detail"`  // 展开说明
	BestFor  string `json:"bestFor"` // 什么场景选它
}

// strategyAdvices 返回三种策略的消耗画像。
func strategyAdvices() []StrategyAdvice {
	return []StrategyAdvice{
		{
			Strategy: "order",
			Token:    "最省",
			Level:    "low",
			Detail: "常态下只用第 1 个目标，也就是只用一个账号，前缀缓存能一直命中。" +
				"只有当它真的失败才换到下一个号，那时才付一次全价重建的代价。" +
				"实测同一会话换不换号能差到 10 倍积分。",
			BestFor: "长会话、连续改代码、一个任务要问几十轮 —— 需要上下文一直热着。",
		},
		{
			Strategy: "round-robin",
			Token:    "最费积分",
			Level:    "high",
			Detail: "每个请求都轮到下一个目标，等于每轮都换账号，" +
				"上次的缓存前缀全部作废，整段上下文按全价重算（约从 10% 变 100%）。" +
				"组里目标越多、上下文越长，烧得越狠；跨供应商时连上游都换了，一样全价。",
			BestFor: "只适合短问短答、单轮即弃的批量任务；或者多个号都逼近限流、宁可烧积分也要保住可用性。",
		},
		{
			Strategy: "manual",
			Token:    "省，但额度挤在一个号上",
			Level:    "mid",
			Detail: "固定用你指定的那个目标，账号不变，缓存最稳。" +
				"代价是积分全压在它一个人身上，用完被迫换的那一次照样要全价重建缓存。",
			BestFor: "某个号额度厚、想集中用它；或长任务想避免中途换号把缓存打碎。",
		},
	}
}

// strategyAdviceOf 取单个策略的建议。
func strategyAdviceOf(key string) *StrategyAdvice {
	for _, a := range strategyAdvices() {
		if a.Strategy == key {
			return &a
		}
	}
	return nil
}

// groupTargetTokenNote 针对具体这一组目标，给出更贴身的积分提示。
//
// 关键视角：每个目标都对应一个账号。所以「目标有几个」直接等于
// 「有几个号会被轮流使用」，轮询的代价随目标数线性放大。
func groupTargetTokenNote(cfg *config.Config, g *config.Group) []string {
	if g == nil || len(g.Targets) == 0 {
		return nil
	}
	var notes []string

	providers := map[string]int{}
	live := 0
	for _, t := range g.Targets {
		pid := t
		if i := strings.IndexByte(t, '/'); i > 0 {
			pid = t[:i]
		}
		providers[pid]++
		if _, _, _, err := provider.Resolve(cfg, t); err == nil {
			live++
		}
	}

	// 最重要的一条：目标数 = 会被轮流使用的账号数
	if len(g.Targets) > 1 {
		if normStrategy(g.Strategy) == "round-robin" {
			notes = append(notes, fmt.Sprintf(
				"这组有 %d 个目标，轮询等于在 %d 个账号之间来回换："+
					"每换一次缓存前缀就作废，整段上下文按全价重算。目标越多，积分消耗越高。",
				len(g.Targets), len(g.Targets)))
		} else {
			notes = append(notes, fmt.Sprintf(
				"这组有 %d 个目标，但常态只用第 1 个账号；"+
					"只有它失败才会换号，那一次的上下文要按全价重算。",
				len(g.Targets)))
		}
	}

	if len(providers) == 1 {
		notes = append(notes, "目标都在同一个供应商内：换目标仍然是换账号，缓存同样会失效 —— 省不省取决于换号的频率，不是供应商是否相同。")
	} else {
		notes = append(notes,
			fmt.Sprintf("这组跨了 %d 个供应商：换目标既是换号又是换上游，缓存必然重建，"+
				"长上下文下这一下可能比省下的额度更贵。", len(providers)))
	}

	if live == 0 {
		notes = append(notes, "注意：现在没有一个目标能解析出来，请求必然失败。")
	} else if live < len(g.Targets) {
		notes = append(notes, fmt.Sprintf(
			"%d 个目标里有 %d 个已失效，顺延链会先撞空再往后走，建议删掉死链。",
			len(g.Targets), len(g.Targets)-live))
	}

	if normStrategy(g.Strategy) != "manual" && len(g.Targets) > 0 {
		notes = append(notes, fmt.Sprintf(
			"排在第 1 位的 %s 是常态下的实际使用者，它的额度决定这组的续航。", g.Targets[0]))
	}
	return notes
}

// normStrategy 把空策略规范成默认值，避免面板拿到空串时找不到建议。
func normStrategy(s string) string {
	switch s {
	case "round-robin", "manual", "order":
		return s
	default:
		return "order"
	}
}
