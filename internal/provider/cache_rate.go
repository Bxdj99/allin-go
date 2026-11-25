package provider

import (
	"sync"
	"sync/atomic"
)

// 账号缓存命中率跟踪。
//
// 为什么需要：保底（余额低于门槛就换号）和缓存命中是直接冲突的 ——
// 换号意味着新账号没有该会话的前缀缓存，整段输入从「按 10% 计价」变成全价。
//
// 实测（2026-11-26，同一会话 54.9 万 token）：
//   留在原账号（余额 47，命中 99.9%）→ 等效约 5.5 万 token
//   切到新账号（余额 1500，命中 0%）  → 实际 54.9 万 token
//
// 为了保住 47 积分而多花 10 倍 token，这明显是亏的。
// 所以：命中率高的账号，余额跌破门槛时也应当继续用。

type cacheRateStat struct {
	hits   atomic.Int64 // 命中的读缓存 token
	prompt atomic.Int64 // 输入总量
	known  atomic.Int64 // 报过缓存字段的请求数
}

var (
	cacheRateMu sync.RWMutex
	cacheRate   = map[string]*cacheRateStat{}
)

// recordCacheRate 记一次请求的缓存命中情况。
func recordCacheRate(pid, aid string, cacheRead, prompt int64, known bool) {
	if aid == "" || !known {
		return
	}
	k := key(pid, aid)
	cacheRateMu.RLock()
	st := cacheRate[k]
	cacheRateMu.RUnlock()
	if st == nil {
		cacheRateMu.Lock()
		if st = cacheRate[k]; st == nil {
			st = &cacheRateStat{}
			cacheRate[k] = st
		}
		cacheRateMu.Unlock()
	}
	st.hits.Add(cacheRead)
	st.prompt.Add(prompt)
	st.known.Add(1)
}

// CacheHealth 返回账号近期缓存命中率（0~1）与是否有数据。
func (p *Pool) CacheHealth(pid, aid string) (float64, bool) {
	k := key(pid, aid)
	cacheRateMu.RLock()
	st := cacheRate[k]
	cacheRateMu.RUnlock()
	if st == nil || st.known.Load() == 0 {
		return 0, false
	}
	prompt := st.prompt.Load()
	if prompt <= 0 {
		return 0, false
	}
	return float64(st.hits.Load()) / float64(prompt), true
}

// resetCacheRate 在账号被换掉/重置时清空它的命中率统计。
func resetCacheRate(pid, aid string) {
	k := key(pid, aid)
	cacheRateMu.Lock()
	delete(cacheRate, k)
	cacheRateMu.Unlock()
}
