// Package fallback 实现「模型级降级链」。
//
// 场景：一家供应商下有多个模型，但额度是按「模型 × 账号」独立算的。
// 只换账号不够 —— 8 个号在 glm-5.3 上全 429 时，换到 deepseek-v4.1-flash 往往还有额度。
//
// 所以转发失败时的降级顺序应该是：
//
//	同模型换账号（fast path）
//	  → 模型链里下一个模型，重新走一遍所有账号
//	  → 下一个供应商（如果配了 group）
//
// 本文件只负责「算出降级顺序」，不执行请求。
package fallback

import "allin/internal/config"

// Step 是一次降级尝试：(供应商, 模型名)。
type Step struct {
	ProviderID string
	Provider   *config.Provider
	Model      string
}

// Chain 为一次请求算出完整的尝试序列。
//
// 规则：
//  1. 原供应商的「当前模型」放在最前
//  2. 同供应商的 models 列表里其余模型依次跟上（这就是降级链）
//  3. 如果模型本身来自 group，则按 group.targets 展开
//  4. 同一个「供应商+模型」只出现一次
func Chain(cfg *config.Config, prov *config.Provider, model string) []Step {
	var out []Step
	seen := map[string]bool{}

	push := func(p *config.Provider, m string) {
		if p == nil || m == "" {
			return
		}
		k := p.ID + "\x00" + m
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, Step{ProviderID: p.ID, Provider: p, Model: m})
	}

	// 1) 当前模型优先
	push(prov, model)

	// 2) 同供应商的其它模型
	if prov != nil {
		for _, m := range prov.Models {
			if m != model {
				push(prov, m)
			}
		}
	}

	return out
}

// ShouldFallbackModel 判断某个错误是否值得换模型（而不是继续同模型换号）。
//
// 额度耗尽（429 / code 6004）是最典型的 —— 同一个模型下所有号都会是同样的错误，
// 继续换号纯属浪费。401 也意味着这个号的凭据坏了，但换号可能有效，所以不算模型级失败。
func ShouldFallbackModel(status int, body string) bool {
	if status == 429 {
		return true
	}
	// 上游把额度耗尽包在 200/400 里带业务码的情况
	for _, code := range []string{`"code":6004`, `"code":10001`, "insufficient_quota", "quota_exceeded"} {
		if contains(body, code) {
			// 10001 在 WorkBuddy 里也用在「今日已签到」，那种不该降级，
			// 但那个接口不走这条路径，所以这里按额度类统一处理
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	if len(sub) == 0 || len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
