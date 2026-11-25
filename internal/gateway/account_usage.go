package gateway

import (
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// 账号级积分统计。
//
// 背景：有用户反馈「一中午 2 万积分没了、三个号全空，感觉被偷了」。
// 网关层最该做的是能自证：每个账号到底花了多少积分、多少次请求、
// 多少 token，全记下来，随时可查。
//
// 上游响应里的 usage.credit 就是本次消耗（实测 WorkBuddy 会返回，如 0.02）。
// 拿不到 credit 的供应商就只记请求数与 token，credit 记 0。

// accountUsage 是单个账号的累计消耗。
type accountUsage struct {
	Requests  atomic.Int64
	Errors    atomic.Int64
	InTokens  atomic.Int64
	OutTokens atomic.Int64
	// CreditMilli 是积分消耗的千分之一单位（整数存，避免浮点累加误差）。
	// 显示时除以 1000。
	CreditMilli atomic.Int64
	LastAt      atomic.Int64 // unix 秒

	// 缓存统计。CacheKnown 记录「有多少次请求上游报过缓存字段」，
	// 作为命中率的分母覆盖率 —— 全都没报时命中率显示「—」而不是 0%。
	CacheRead   atomic.Int64
	CacheWrite  atomic.Int64
	CacheKnown  atomic.Int64
	PromptTotal atomic.Int64 // 报过缓存字段的那些请求的输入总量
}

var (
	accUsageMu    sync.RWMutex
	accUsageTable = map[string]*accountUsage{} // key: providerID/accountID
)

func accUsageKey(providerID, accountID string) string {
	if providerID == "" {
		providerID = "unknown"
	}
	if accountID == "" {
		accountID = "unknown"
	}
	return providerID + "/" + accountID
}

// recordAccountUsage 记一次账号级消耗。
func recordAccountUsage(providerID, accountID string, ok bool, u usageTokens) {
	if accountID == "" {
		return
	}
	k := accUsageKey(providerID, accountID)
	accUsageMu.RLock()
	st := accUsageTable[k]
	accUsageMu.RUnlock()
	if st == nil {
		accUsageMu.Lock()
		if st = accUsageTable[k]; st == nil {
			st = &accountUsage{}
			accUsageTable[k] = st
		}
		accUsageMu.Unlock()
	}

	st.Requests.Add(1)
	if !ok {
		st.Errors.Add(1)
	}
	st.InTokens.Add(int64(u.In))
	st.OutTokens.Add(int64(u.Out))
	if u.Credit > 0 {
		// 转成千分之一累加，四舍五入
		st.CreditMilli.Add(int64(u.Credit*1000 + 0.5))
	}
	st.LastAt.Store(time.Now().Unix())

	// 缓存统计：只有上游真的报了缓存字段才计入，
	// 保证命中率的分母只覆盖「有数据」的那部分请求。
	if u.HasCacheInfo() {
		st.CacheKnown.Add(1)
		st.CacheRead.Add(u.CacheRead)
		st.CacheWrite.Add(u.CacheWrite)
		st.PromptTotal.Add(int64(u.In))
	}
	markAccUsageDirty()
}

// accountUsageRow 是导出给面板的一行。
type accountUsageRow struct {
	ProviderID string  `json:"providerId"`
	AccountID  string  `json:"accountId"`
	Requests   int64   `json:"requests"`
	Errors     int64   `json:"errors"`
	InTokens   int64   `json:"inTokens"`
	OutTokens  int64   `json:"outTokens"`
	Credit     float64 `json:"credit"`
	LastAt     int64   `json:"lastAt,omitempty"`

	// 缓存命中相关。HitRate 为 nil 表示数据不足，界面应显示「—」。
	CacheRead  int64    `json:"cacheRead,omitempty"`
	CacheWrite int64    `json:"cacheWrite,omitempty"`
	CacheKnown int64    `json:"cacheKnown,omitempty"`
	HitRate    *float64 `json:"hitRate,omitempty"`
}

// accountUsageRows 导出全部账号的累计消耗，按积分从高到低排。
func accountUsageRows() []accountUsageRow {
	accUsageMu.RLock()
	defer accUsageMu.RUnlock()

	rows := make([]accountUsageRow, 0, len(accUsageTable))
	for k, st := range accUsageTable {
		pid, aid := splitAccKey(k)
		row := accountUsageRow{
			ProviderID: pid,
			AccountID:  aid,
			Requests:   st.Requests.Load(),
			Errors:     st.Errors.Load(),
			InTokens:   st.InTokens.Load(),
			OutTokens:  st.OutTokens.Load(),
			Credit:     float64(st.CreditMilli.Load()) / 1000,
			LastAt:     st.LastAt.Load(),
			CacheRead:  st.CacheRead.Load(),
			CacheWrite: st.CacheWrite.Load(),
			CacheKnown: st.CacheKnown.Load(),
		}
		// 命中率只在「有请求报过缓存字段」时才给值。
		// 用累计口径：命中的读缓存量 / 这些请求的输入总量。
		// 没有数据时保持 nil，界面显示「—」—— 而不是骗人的 0%。
		if st.CacheKnown.Load() > 0 {
			if denom := st.PromptTotal.Load(); denom > 0 {
				rate := clampRatio(float64(st.CacheRead.Load()) / float64(denom))
				row.HitRate = &rate
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Credit != rows[j].Credit {
			return rows[i].Credit > rows[j].Credit
		}
		return rows[i].Requests > rows[j].Requests
	})
	return rows
}

func splitAccKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// resetAccountUsage 清零统计（面板上的「清零」按钮用）。
func resetAccountUsage() {
	accUsageMu.Lock()
	accUsageTable = map[string]*accountUsage{}
	accUsageMu.Unlock()
	markAccUsageDirty()
}

// handleAccountUsage 返回按账号统计的积分消耗。
//
//	GET  /api/accounts/usage          看累计
//	POST /api/accounts/usage/reset    清零
func (s *Server) handleAccountUsage(w http.ResponseWriter, r *http.Request) {
	rows := accountUsageRows()
	var totalCredit float64
	var totalReq int64
	for _, row := range rows {
		totalCredit += row.Credit
		totalReq += row.Requests
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"rows":        rows,
		"totalCredit": round2f(totalCredit),
		"totalReq":    totalReq,
		"note":        "credit 取自上游响应的 usage.credit；上游不给的供应商记 0",
	})
}

// handleAccountUsageReset 清零账号级统计。
func (s *Server) handleAccountUsageReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	resetAccountUsage()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// recordCreditDelta 把「余额下降量」记到某个账号的消耗上。
//
// 与 recordAccountUsage 分开：那个按请求记 token 与 credit 字段，
// 这个按余额差值记积分。两者互不覆盖。
func (s *Server) recordCreditDelta(providerID, accountID string, delta float64) {
	if delta <= 0 || accountID == "" {
		return
	}
	k := accUsageKey(providerID, accountID)
	accUsageMu.RLock()
	st := accUsageTable[k]
	accUsageMu.RUnlock()
	if st == nil {
		accUsageMu.Lock()
		if st = accUsageTable[k]; st == nil {
			st = &accountUsage{}
			accUsageTable[k] = st
		}
		accUsageMu.Unlock()
	}
	st.CreditMilli.Add(int64(delta*1000 + 0.5))
	markAccUsageDirty()
}

// clampRatio 把比例夹在 [0,1]，并把 NaN 归零。
func clampRatio(v float64) float64 {
	if v != v { // NaN
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
