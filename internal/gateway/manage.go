package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"allin/internal/config"
)

// ---------- 用量统计（内存计数，进程重启归零） ----------
//
// 两个维度：
//   - 累计：usageTable，key = providerID/model
//   - 按天：usageDaily，key = 日期 → providerID/model
//
// 写入只发生在 gateway 层（relay / relayXxx / forwardWebChat / forwardDeepSeek / forward 的失败分支），
// 供应商包不参与统计。

// usageStat 是一组计数（累计或某一天的）。
type usageStat struct {
	Requests  atomic.Int64
	Errors    atomic.Int64
	InTokens  atomic.Int64
	OutTokens atomic.Int64
	LastAt    atomic.Int64 // unix 秒
}

// usageKeepDays 是按天聚合保留的天数（含今天）。
const usageKeepDays = 7

var (
	usageMu    sync.RWMutex
	usageTable = map[string]*usageStat{}            // key: providerID/model
	usageDaily = map[string]map[string]*usageStat{} // 日期(yyyy-mm-dd) → key → stat
)

// usageKeyOf 拼出统计键 providerID/model；空值兜底成 unknown，
// 免得「a/」和「a/b」混进同一行。
func usageKeyOf(providerID, model string) string {
	if providerID == "" {
		providerID = "unknown"
	}
	if model == "" {
		model = "unknown"
	}
	return providerID + "/" + model
}

// usageDay 返回 unix 秒对应的本地日期（yyyy-mm-dd）。
func usageDay(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02")
}

// usageOfLocked 取（必要时创建）累计表里的某个键。调用方必须持有 usageMu 写锁。
func usageOfLocked(key string) *usageStat {
	if st := usageTable[key]; st != nil {
		return st
	}
	st := &usageStat{}
	usageTable[key] = st
	return st
}

// usageOfDay 取（必要时创建）某一天的某个键。调用方必须持有 usageMu 写锁。
func usageOfDay(day, key string, now int64) *usageStat {
	dayTable := usageDaily[day]
	if dayTable == nil {
		dayTable = map[string]*usageStat{}
		usageDaily[day] = dayTable
		pruneUsageDays(now)
	}
	st := dayTable[key]
	if st == nil {
		st = &usageStat{}
		dayTable[key] = st
	}
	return st
}

// pruneUsageDays 删掉超出保留窗口的日期桶。调用方必须持有 usageMu 写锁。
func pruneUsageDays(now int64) {
	if len(usageDaily) <= usageKeepDays {
		return
	}
	cut := usageDay(now - int64(usageKeepDays-1)*86400)
	for day := range usageDaily {
		if day < cut {
			delete(usageDaily, day)
		}
	}
}

// usageCounts 是一次请求要落账的全部数字。
type usageCounts struct {
	InTokens  int
	OutTokens int
	Total     int // 上游只给 total_tokens 时的兜底（DeepSeek 网页版只有这一项）
}

// addUsage 把一次请求同时记进累计表与当天的日期桶。
// tokens 拿不到就记 0，但请求数（和错误数）一定会记。
func addUsage(providerID, model string, ok bool, c usageCounts) {
	key := usageKeyOf(providerID, model)
	now := time.Now()
	ts := now.Unix()
	day := usageDay(ts)

	usageMu.Lock()
	for _, st := range []*usageStat{usageOfLocked(key), usageOfDay(day, key, ts)} {
		st.Requests.Add(1)
		if !ok {
			st.Errors.Add(1)
		}
		if c.InTokens > 0 {
			st.InTokens.Add(int64(c.InTokens))
		}
		if c.OutTokens > 0 {
			st.OutTokens.Add(int64(c.OutTokens))
		}
		st.LastAt.Store(ts)
	}
	usageMu.Unlock()
}

// recordUsage 记录一次请求。tokens 由上游 usage 字段提供，拿不到就记 0。
func recordUsage(providerID, model string, ok bool, inTok, outTok int) {
	addUsage(providerID, model, ok, usageCounts{InTokens: inTok, OutTokens: outTok})
}

// recordUsageStat 记录一次请求（计数已装箱）。
func recordUsageStat(providerID, model string, ok bool, c usageCounts) {
	addUsage(providerID, model, ok, c)
}

// usageStatView 把一组计数摊平成面板/接口好用的形状。
func usageStatView(key string, st *usageStat) map[string]any {
	return map[string]any{
		"key":       key,
		"requests":  st.Requests.Load(),
		"errors":    st.Errors.Load(),
		"inTokens":  st.InTokens.Load(),
		"outTokens": st.OutTokens.Load(),
		"lastAt":    st.LastAt.Load(),
	}
}

// handleUsage 返回用量表（累计 + 最近 7 天按天）。
//
//	GET /api/usage           {ok, usage:[{key,requests,…}], daily:[{day,rows:[…]}], summary:{…}}
//	GET /api/usage?days=3    只返回最近 3 天的按天数据（累计始终全量）
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	dayLimit := usageKeepDays
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < dayLimit {
			dayLimit = n
		}
	}

	usageMu.RLock()
	keys := make([]string, 0, len(usageTable))
	for k := range usageTable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, usageStatView(k, usageTable[k]))
	}

	days := make([]string, 0, len(usageDaily))
	for d := range usageDaily {
		days = append(days, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	if len(days) > dayLimit {
		days = days[:dayLimit]
	}
	daily := make([]map[string]any, 0, len(days))
	for _, d := range days {
		dayTable := usageDaily[d]
		dayKeys := make([]string, 0, len(dayTable))
		for k := range dayTable {
			dayKeys = append(dayKeys, k)
		}
		sort.Strings(dayKeys)
		items := make([]map[string]any, 0, len(dayKeys))
		for _, k := range dayKeys {
			items = append(items, usageStatView(k, dayTable[k]))
		}
		daily = append(daily, map[string]any{"day": d, "rows": items})
	}
	summary := s.usageSummaryLocked()
	usageMu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"keepDays": usageKeepDays,
		"usage":    rows,
		"daily":    daily,
		"summary":  summary,
	})
}

// handleUsageReset 清空用量表。
func (s *Server) handleUsageReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	usageMu.Lock()
	usageTable = map[string]*usageStat{}
	usageDaily = map[string]map[string]*usageStat{}
	usageMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// successRate 返回 0~1 的成功率；没有请求时返回 0（调用方自己判断 requests==0）。
func successRate(req, errs int64) float64 {
	if req <= 0 {
		return 0
	}
	if errs < 0 {
		errs = 0
	}
	if errs > req {
		errs = req
	}
	return float64(req-errs) / float64(req)
}

// usageSummaryLocked 汇总累计表：总量 + 按供应商分组 + 按模型 + 按天趋势。
//
// 调用方必须持有 usageMu（读锁即可）。s 可以为 nil。
func (s *Server) usageSummaryLocked() map[string]any {
	providerOf := func(k string) string {
		if i := strings.Index(k, "/"); i >= 0 {
			return k[:i]
		}
		return k
	}

	var totalReq, totalErr, totalIn, totalOut, totalLast int64
	type agg struct{ req, errs, in, out, last int64 }
	perProvider := map[string]*agg{}
	perModel := map[string]map[string]any{}

	keys := make([]string, 0, len(usageTable))
	for k := range usageTable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		st := usageTable[k]
		req, errs := st.Requests.Load(), st.Errors.Load()
		in, out, last := st.InTokens.Load(), st.OutTokens.Load(), st.LastAt.Load()
		totalReq += req
		totalErr += errs
		totalIn += in
		totalOut += out
		if last > totalLast {
			totalLast = last
		}
		pid := providerOf(k)
		a := perProvider[pid]
		if a == nil {
			a = &agg{}
			perProvider[pid] = a
		}
		a.req += req
		a.errs += errs
		a.in += in
		a.out += out
		if last > a.last {
			a.last = last
		}
		model := k
		if strings.HasPrefix(k, pid+"/") {
			model = k[len(pid)+1:]
		} else {
			model = ""
		}
		perModel[k] = map[string]any{
			"key":         k,
			"provider":    pid,
			"model":       model,
			"requests":    req,
			"errors":      errs,
			"inTokens":    in,
			"outTokens":   out,
			"successRate": successRate(req, errs),
		}
	}

	pids := make([]string, 0, len(perProvider))
	for p := range perProvider {
		pids = append(pids, p)
	}
	sort.Strings(pids)
	providers := make([]map[string]any, 0, len(pids))
	for _, p := range pids {
		a := perProvider[p]
		providers = append(providers, map[string]any{
			"provider":    p,
			"requests":    a.req,
			"errors":      a.errs,
			"inTokens":    a.in,
			"outTokens":   a.out,
			"totalTokens": a.in + a.out,
			"successRate": successRate(a.req, a.errs),
			"lastAt":      a.last,
		})
	}

	modelKeys := make([]string, 0, len(perModel))
	for k := range perModel {
		modelKeys = append(modelKeys, k)
	}
	sort.Strings(modelKeys)
	models := make([]map[string]any, 0, len(modelKeys))
	for _, k := range modelKeys {
		models = append(models, perModel[k])
	}

	// 按天趋势：日期升序，只带总量（面板画折线用）
	days := make([]string, 0, len(usageDaily))
	for d := range usageDaily {
		days = append(days, d)
	}
	sort.Strings(days)
	trend := make([]map[string]any, 0, len(days))
	for _, d := range days {
		var req, errs, in, out int64
		for _, st := range usageDaily[d] {
			req += st.Requests.Load()
			errs += st.Errors.Load()
			in += st.InTokens.Load()
			out += st.OutTokens.Load()
		}
		trend = append(trend, map[string]any{
			"day": d, "requests": req, "errors": errs,
			"inTokens": in, "outTokens": out, "successRate": successRate(req, errs),
		})
	}

	out := map[string]any{
		"requests":    totalReq,
		"errors":      totalErr,
		"okRequests":  totalReq - totalErr,
		"inTokens":    totalIn,
		"outTokens":   totalOut,
		"totalTokens": totalIn + totalOut,
		"successRate": successRate(totalReq, totalErr),
		"providers":   providers,
		"models":      models,
		"trend":       trend,
		"lastAt":      totalLast,
		"keepDays":    usageKeepDays,
	}
	if s != nil {
		out["uptimeSec"] = int64(time.Since(s.started).Seconds())
		out["startedAt"] = s.started.Unix()
		out["gatewayRequests"] = s.requests.Load()
	}
	return out
}

// handleUsageSummary 返回总量统计（总请求、总 token、成功率、按供应商分组、按天趋势）。
//
//	GET /api/usage/summary
func (s *Server) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	usageMu.RLock()
	out := s.usageSummaryLocked()
	usageMu.RUnlock()
	out["ok"] = true
	writeJSON(w, http.StatusOK, out)
}

// ---------- 供应商管理 ----------

// handleProviders 列出全部供应商（凭据脱敏）。
//
//	GET /api/providers
//
// models / exposed / exposedModels 三个字段的分工：
//
//	models       同步到的全部模型（配置里的 models 原样）
//	exposedModels 面板勾选结果原样；为空 = 「不限制，全部暴露」（老配置）
//	exposed      实际对外生效的列表（已按 models 过滤过），面板块勾选状态以它为准
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	out := make([]map[string]any, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p == nil {
			continue
		}
		accts := make([]map[string]any, 0, len(p.Accounts))
		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			row := map[string]any{
				"id":      a.ID,
				"name":    a.Name,
				"type":    a.Type,
				"enabled": a.IsEnabled(),
			}
			if k := a.Key(); k != "" {
				row["keyPreview"] = maskKey(k)
			}
			if a.Expires > 0 {
				row["expires"] = a.Expires
			}
			if a.CheckinDate != "" {
				row["checkinDate"] = a.CheckinDate
				row["checkinOutcome"] = a.CheckinOutcome
				row["checkinCredit"] = a.CheckinCredit
			}
			accts = append(accts, row)
		}
		out = append(out, map[string]any{
			"id":             p.ID,
			"name":           p.Name,
			"kind":           p.Kind,
			"baseUrl":        p.BaseURL,
			"enabled":        p.IsEnabled(),
			"models":         p.Models,
			"exposedModels":  p.ExposedModels,
			"exposed":        p.Exposed(),
			"autoSyncModels": p.AutoSyncModels,
			"accounts":       accts,
			"notes":          p.Notes,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"providers": out,
		"kinds": []string{
			"openai", "anthropic", "workbuddy", "trae", "copilot", "codex",
			"deepseek-web", "qoder", "raccoon", "loomy", "cline", "minimax",
			"catpaw", "wenxin", "antigravity",
		},
	})
}

// handleProvidersOrWrite 按方法分派：GET 列表，POST/DELETE 走写操作。
func (s *Server) handleProvidersOrWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleProviders(w, r)
		return
	}
	s.handleProviderWrite(w, r)
}

// providerPayload 是新增/更新供应商的请求体。
type providerPayload struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	BaseURL string   `json:"baseUrl"`
	APIKey  string   `json:"apiKey"`
	Models  []string `json:"models"`
	Enabled *bool    `json:"enabled"`
	// AutoSyncModels 仅用指针区分「没传」（保持原值）与「显式设为 false」。
	AutoSyncModels *bool `json:"autoSyncModels"`
	// RouteMode 是账号路由策略：sticky / rotate / balance / expiry。
	RouteMode string `json:"routeMode"`
	Delete    bool   `json:"delete"`
}

// handleProviderWrite 新增、更新或删除一个供应商。
//
//	POST   /api/providers   新增/更新
//	DELETE /api/providers?id=xxx   删除
func (s *Server) handleProviderWrite(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()

	if r.Method == http.MethodDelete {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 id"})
			return
		}
		kept := make([]*config.Provider, 0, len(cfg.Providers))
		found := false
		for _, p := range cfg.Providers {
			if p != nil && p.ID == id {
				found = true
				continue
			}
			kept = append(kept, p)
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "供应商不存在"})
			return
		}
		cfg.Providers = kept
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
		return
	}

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST/DELETE"})
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取失败"})
		return
	}
	var in providerPayload
	if err := json.Unmarshal(raw, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
		return
	}
	in.ID = strings.TrimSpace(in.ID)
	if in.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "id 不能为空"})
		return
	}

	p := cfg.FindProvider(in.ID)
	created := false
	if p == nil {
		p = &config.Provider{ID: in.ID}
		cfg.Providers = append(cfg.Providers, p)
		created = true
	}
	if in.Name != "" {
		p.Name = in.Name
	}
	if in.Kind != "" {
		p.Kind = in.Kind
	}
	if in.RouteMode != "" {
		switch in.RouteMode {
		case config.RouteSticky, config.RouteRotate, config.RouteBalance, config.RouteExpiry:
			p.RouteMode = in.RouteMode
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": "routeMode 只能是 sticky / rotate / balance / expiry",
			})
			return
		}
	}
	if in.BaseURL != "" {
		p.BaseURL = in.BaseURL
	}
	if in.APIKey != "" && !strings.Contains(in.APIKey, "...") {
		p.APIKey = in.APIKey
	}
	if in.Models != nil {
		p.Models = in.Models
	}
	if in.Enabled != nil {
		p.Enabled = in.Enabled
	}
	if in.AutoSyncModels != nil {
		p.AutoSyncModels = *in.AutoSyncModels
	}

	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "created": created, "id": p.ID})
}

// ---------- 账号管理 ----------

// accountPayload 是账号增改的请求体。
type accountPayload struct {
	ProviderID string `json:"providerId"`
	AccountID  string `json:"accountId"`
	Name       string `json:"name"`
	Access     string `json:"access"`
	Refresh    string `json:"refresh"`
	UID        string `json:"uid"`
	Domain     string `json:"domain"`
	Realm      string `json:"realm"`
	Enabled    *bool  `json:"enabled"`
	Delete     bool   `json:"delete"`
}

// handleAccountWrite 增删改某个供应商下的账号。
//
//	POST   /api/accounts       新增/更新/切换启用
//	DELETE /api/accounts?providerId=x&accountId=y
func (s *Server) handleAccountWrite(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()

	if r.Method == http.MethodDelete {
		pid := strings.TrimSpace(r.URL.Query().Get("providerId"))
		aid := strings.TrimSpace(r.URL.Query().Get("accountId"))
		p := cfg.FindProvider(pid)
		if p == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "供应商不存在"})
			return
		}
		kept := make([]*config.Account, 0, len(p.Accounts))
		found := false
		for _, a := range p.Accounts {
			if a != nil && a.ID == aid {
				found = true
				continue
			}
			kept = append(kept, a)
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "账号不存在"})
			return
		}
		p.Accounts = kept
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": aid})
		return
	}

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST/DELETE"})
		return
	}

	var in accountPayload
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
		return
	}
	p := cfg.FindProvider(strings.TrimSpace(in.ProviderID))
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "供应商不存在"})
		return
	}

	// 找到已有账号（按 id）或新建
	var acc *config.Account
	for _, a := range p.Accounts {
		if a != nil && a.ID == in.AccountID && in.AccountID != "" {
			acc = a
			break
		}
	}
	created := false
	if acc == nil {
		acc = &config.Account{ID: in.AccountID}
		if acc.ID == "" {
			acc.ID = p.ID + "-" + shortID()
		}
		acc.AddedAt = time.Now().UnixMilli()
		p.Accounts = append(p.Accounts, acc)
		created = true
	}
	if in.Name != "" {
		acc.Name = in.Name
	}
	if in.Access != "" && !strings.Contains(in.Access, "...") {
		acc.Access = in.Access
		if acc.Type == "" {
			acc.Type = "token"
		}
	}
	if in.Refresh != "" && !strings.Contains(in.Refresh, "...") {
		acc.Refresh = in.Refresh
	}
	if in.UID != "" {
		acc.UID = in.UID
	}
	if in.Domain != "" {
		acc.Domain = in.Domain
	}
	if in.Realm != "" {
		acc.Realm = in.Realm
	}
	if in.Enabled != nil {
		acc.Enabled = in.Enabled
	}

	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "created": created, "accountId": acc.ID})
}

// ---------- 小工具 ----------

func maskKey(k string) string {
	if len(k) <= 12 {
		return "***"
	}
	return k[:6] + "..." + k[len(k)-4:]
}

func shortID() string {
	const alphabet = "0123456789abcdef"
	b := make([]byte, 8)
	for i := range b {
		b[i] = alphabet[time.Now().UnixNano()>>uint(i*3)&0xf]
	}
	// 混入纳秒时间，够唯一
	return string(b)
}
