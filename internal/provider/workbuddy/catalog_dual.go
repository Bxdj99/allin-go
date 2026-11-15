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
// **必须严格按账号 realm 走**，不能跨站合并 —— 跨站的模型名字在本站不存在，
// 调用会返回 11102 / 11158，用户以为能用实际全废。
// 同一站内仍然并发探 /v3/config 与企业端点，这两个是真互补的。

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

	// 只探账号自己所属的站。
	//
	// 历史教训（v1.6.1）：曾经试过「两站都探、取并集」，结果国内账号
	// 拿到了国际站独有的模型（gpt-6-astra / grok-4.7 / gemini-4-carbon 等），
	// 这些模型在国内站根本不存在，调用直接 code=11102「service info not found」
	// 或 11158「not allowed for user type」。用户看到一堆模型但大半用不了。
	//
	// 所以现在严格按 realm 走：国内号只拉国内站，国际号只拉国际站。
	primary := OriginFor(origin, acc)
	candidates := []string{primary}

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
