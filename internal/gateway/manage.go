package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"allin/internal/config"
)

// ---------- 用量统计（内存计数，进程重启归零） ----------

// usageKey 是「供应商/模型」维度的统计键。
type usageStat struct {
	Requests  atomic.Int64
	Errors    atomic.Int64
	InTokens  atomic.Int64
	OutTokens atomic.Int64
	LastAt    atomic.Int64 // unix 秒
}

var (
	usageMu    sync.RWMutex
	usageTable = map[string]*usageStat{}
)

func usageOf(key string) *usageStat {
	usageMu.RLock()
	st := usageTable[key]
	usageMu.RUnlock()
	if st != nil {
		return st
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	if st = usageTable[key]; st == nil {
		st = &usageStat{}
		usageTable[key] = st
	}
	return st
}

// recordUsage 记录一次请求。tokens 由上游 usage 字段提供，拿不到就记 0。
func recordUsage(providerID, model string, ok bool, inTok, outTok int) {
	st := usageOf(providerID + "/" + model)
	st.Requests.Add(1)
	if !ok {
		st.Errors.Add(1)
	}
	if inTok > 0 {
		st.InTokens.Add(int64(inTok))
	}
	if outTok > 0 {
		st.OutTokens.Add(int64(outTok))
	}
	st.LastAt.Store(time.Now().Unix())
}

// handleUsage 返回用量表。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	usageMu.RLock()
	keys := make([]string, 0, len(usageTable))
	for k := range usageTable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		st := usageTable[k]
		rows = append(rows, map[string]any{
			"key":       k,
			"requests":  st.Requests.Load(),
			"errors":    st.Errors.Load(),
			"inTokens":  st.InTokens.Load(),
			"outTokens": st.OutTokens.Load(),
			"lastAt":    st.LastAt.Load(),
		})
	}
	usageMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "usage": rows})
}

// handleUsageReset 清空用量表。
func (s *Server) handleUsageReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	usageMu.Lock()
	usageTable = map[string]*usageStat{}
	usageMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 供应商管理 ----------

// handleProviders 列出全部供应商（凭据脱敏）。
//
//	GET /api/providers
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
			"id":       p.ID,
			"name":     p.Name,
			"kind":     p.Kind,
			"baseUrl":  p.BaseURL,
			"enabled":  p.IsEnabled(),
			"models":   p.Models,
			"accounts": accts,
			"notes":    p.Notes,
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
	Delete  bool     `json:"delete"`
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
