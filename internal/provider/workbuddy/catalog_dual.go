package workbuddy

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// 双域名目录拉取。
//
// 背景（2026-11 实测）：
//
//	国内站 copilot.tencent.com + CLI UA → 59 个模型
//	国内站 copilot.tencent.com + IDE UA → 24 个
//	国际站 www.workbuddy.ai   + CLI UA → 25 个
//	国际站 www.workbuddy.ai   + IDE UA → 12 个
//
// 账号的 realm 只决定「默认用哪个站」，不代表另一个站拿不到东西。
// 所以这里两站都探一遍取并集：国际号也能用国内站的完整目录
// （能不能真正调通是另一回事，但至少模型列表要全，用户可以选择试）。

// CatalogProbe 是一次探测的明细，供诊断接口展示。
type CatalogProbe struct {
	Origin string `json:"origin"`
	UA     string `json:"ua"`     // cli / ide
	Count  int    `json:"count"`  // 拿到的原始条目数（过滤前）
	Kept   int    `json:"kept"`   // 过滤后保留的对话模型数
	Error  string `json:"error,omitempty"`
}

// CatalogResult 是完整结果。
type CatalogResult struct {
	Models []ModelInfo    `json:"-"` // 合并后的模型（已过滤）
	Probes []CatalogProbe `json:"probes"`
	// Merged 表示是否真的用了两个域名
	Merged bool `json:"merged"`
}

// FetchCatalogWithProbes 拉取模型目录，并把每次探测的明细带回来。
func FetchCatalogWithProbes(ctx context.Context, origin string, acc *Account) (*CatalogResult, error) {
	if acc == nil {
		return nil, fmt.Errorf("账号为空")
	}
	if strings.TrimSpace(acc.Token) == "" {
		return nil, fmt.Errorf("账号缺少访问令牌")
	}

	// 候选站点：优先账号自己的站，再补另一个站
	primary := OriginFor(origin, acc)
	var candidates []string
	candidates = append(candidates, primary)
	if !strings.Contains(primary, "workbuddy.ai") {
		candidates = append(candidates, "https://www.workbuddy.ai")
	} else {
		candidates = append(candidates, "https://copilot.tencent.com")
	}

	type probeResult struct {
		base  string
		infos []ModelInfo
		probe CatalogProbe
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []probeResult
	)
	for _, base := range candidates {
		// 每个站：v3/config 与企业端点并发
		for _, kind := range []string{"v3", "enterprise"} {
			wg.Add(1)
			go func(b, k string) {
				defer wg.Done()
				var (
					infos []ModelInfo
					err   error
					ua    = "cli"
				)
				if k == "v3" {
					infos, err = probeV3(ctx, b, acc)
				} else {
					infos, err = probeEnterprise(ctx, b, acc)
					ua = "enterprise"
				}
				pr := CatalogProbe{Origin: b, UA: ua, Kept: len(infos)}
				if err != nil {
					pr.Error = err.Error()
				} else {
					// Count 与 Kept 在 probe 内部已过滤，这里两者相同
					pr.Count = len(infos)
				}
				mu.Lock()
				results = append(results, probeResult{base: b, infos: infos, probe: pr})
				mu.Unlock()
			}(base, kind)
		}
	}
	wg.Wait()

	out := &CatalogResult{}
	allFailed := true
	// 先放主场地的结果，再放副站（保证顺序稳定：主站模型在前）
	for _, base := range candidates {
		for _, r := range results {
			if r.base != base {
				continue
			}
			out.Probes = append(out.Probes, r.probe)
			if len(r.infos) > 0 {
				allFailed = false
				out.Models = mergeModelInfos(out.Models, r.infos)
			}
		}
	}
	if allFailed {
		var msgs []string
		for _, p := range out.Probes {
			if p.Error != "" {
				msgs = append(msgs, fmt.Sprintf("%s/%s: %s", shortOrigin(p.Origin), p.UA, p.Error))
			}
		}
		return out, fmt.Errorf("所有目录端点均失败: %s", strings.Join(msgs, "；"))
	}
	out.Merged = len(candidates) > 1
	return out, nil
}

func shortOrigin(o string) string {
	o = strings.TrimPrefix(o, "https://")
	o = strings.TrimPrefix(o, "http://")
	return o
}
