package gateway

import (
	"net/http"
	"sort"
	"time"

	"allin/internal/config"
)

// 冷却与额度状态的可读化。
//
// 之前 `/api/accounts` 只给一堆数字（failures / coolRemainSec），
// 用户看不出「为什么调不通」。这里把它翻译成人话：
//   - 哪些账号在用，哪些在冷却
//   - 冷却到几点恢复
//   - 整个供应商是不是已经没号可用
//   - 是不是某个模型额度耗尽

// handleHealth 返回人类可读的整体健康状态。
//
//	GET /api/health
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	raw := s.pool.Snapshot()

	// 建索引：providerID/accountID → 状态
	type acctState struct {
		cooling   bool
		remainSec int64
		failures  int
		successes int64
		reason    string
	}
	states := map[string]*acctState{}
	for _, row := range raw {
		k, _ := row["key"].(string)
		st := &acctState{}
		if v, ok := row["failures"].(int); ok {
			st.failures = v
		}
		if v, ok := row["successes"].(int64); ok {
			st.successes = v
		}
		if v, ok := row["cooling"].(bool); ok {
			st.cooling = v
		}
		if v, ok := row["coolRemainSec"].(int64); ok {
			st.remainSec = v
		}
		if v, ok := row["lastFailure"].(string); ok {
			st.reason = v
		}
		states[k] = st
	}

	type acctView struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Enabled     bool   `json:"enabled"`
		State       string `json:"state"` // ok / cooling / unused / disabled
		CoolRemain  int64  `json:"coolRemainSec,omitempty"`
		CoolUntil   string `json:"coolUntil,omitempty"`
		LastFailure string `json:"lastFailure,omitempty"`
	}
	type provView struct {
		ID           string     `json:"id"`
		Name         string     `json:"name"`
		Kind         string     `json:"kind"`
		Enabled      bool       `json:"enabled"`
		State        string     `json:"state"` // ok / partial / exhausted / unconfigured
		Available    int        `json:"available"`
		Total        int        `json:"total"`
		ModelCount   int        `json:"modelCount"`
		Accounts     []acctView `json:"accounts"`
		Note         string     `json:"note,omitempty"`
	}

	var provs []provView
	anyExhausted := false
	anyCooling := false

	for _, p := range cfg.Providers {
		if p == nil {
			continue
		}
		pv := provView{
			ID: p.ID, Name: p.Name, Kind: p.Kind,
			Enabled: p.IsEnabled(), ModelCount: len(p.Models),
		}
		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			av := acctView{ID: a.ID, Name: a.Name, Enabled: a.IsEnabled()}
			st := states[p.ID+"/"+a.ID]
			switch {
			case !a.IsEnabled():
				av.State = "disabled"
			case st == nil:
				av.State = "unused"
				pv.Available++
			case st.cooling:
				av.State = "cooling"
				av.CoolRemain = st.remainSec
				av.CoolUntil = time.Now().Add(time.Duration(st.remainSec) * time.Second).Format("2006-01-02 15:04:05")
				av.LastFailure = st.reason
				anyCooling = true
			default:
				av.State = "ok"
				pv.Available++
			}
			pv.Total++
			pv.Accounts = append(pv.Accounts, av)
		}

		switch {
		case !p.IsEnabled():
			pv.State = "disabled"
		case len(p.Accounts) == 0 && p.APIKey == "":
			pv.State = "unconfigured"
			pv.Note = "还没填 API Key 或登录账号"
		case pv.Available == 0 && pv.Total > 0:
			pv.State = "exhausted"
			pv.Note = "所有账号都在冷却，暂时无可用凭据"
			anyExhausted = true
		case pv.Available < pv.Total:
			pv.State = "partial"
			pv.Note = "部分账号在冷却"
		default:
			pv.State = "ok"
		}
		provs = append(provs, pv)
	}

	// 按状态优先级排序：exhausted → partial → ok → unconfigured
	rank := map[string]int{"exhausted": 0, "partial": 1, "ok": 2, "unconfigured": 3, "disabled": 4}
	sort.SliceStable(provs, func(i, j int) bool {
		return rank[provs[i].State] < rank[provs[j].State]
	})

	// 顶部横幅提示
	banner := ""
	bannerLevel := "ok"
	switch {
	case anyExhausted:
		banner = "有供应商的账号全部在冷却中，调用会失败。等冷却结束，或补充账号。"
		bannerLevel = "err"
	case anyCooling:
		banner = "有账号正在冷却（被上游限流或凭据失效），其余账号仍可用。"
		bannerLevel = "warn"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"banner":      banner,
		"bannerLevel": bannerLevel,
		"providers":   provs,
		"checkedAt":   time.Now().Unix(),
	})
}

// 确保 config 被引用（provView 用到 *config.Provider 的字段）
var _ = config.Provider{}
