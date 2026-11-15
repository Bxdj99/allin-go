// catalog.go —— 模型目录同步：从上游拉一份真实的模型列表，写回配置。
//
// 背景：配置里的 provider.models 是手写的，通常只包含几个常用模型，而上游实际
// 下发的有几十个——用户既看不到也调不到。本文件提供两条接口：
//
//	POST /api/providers/sync            同步并写回配置（providerId 为空则同步全部）
//	GET  /api/providers/catalog         只看不写
//
// 合并策略（两处都适用）：**只追加，不覆盖、不删除**。
//   - 用户手填的模型一律保留，即使上游这次没下发（可能是别名、可能是临时下线）；
//   - 上游新增的追加到末尾；
//   - 不动顺序，不动大小写（模型名大小写敏感）。
//
// 为什么不做成「以上游为准覆盖」：这张表同时是 /v1/models 的暴露清单和路由目标
// 来源，一次上游抖动就把用户精心筛过的列表清空，代价远大于收益。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"allin/internal/config"
	"allin/internal/provider"
	"allin/internal/provider/openaicatalog"
	"allin/internal/provider/trae"
	"allin/internal/provider/workbuddy"
)

// syncTimeout 是单个供应商一次同步的上限（含两次并发探测）。
const syncTimeout = 40 * time.Second

// syncRow 是单个供应商的同步结果，直接进 HTTP 响应。
type syncRow struct {
	ProviderID string   `json:"providerId"`
	Kind       string   `json:"kind,omitempty"`
	OK         bool     `json:"ok"`
	// Fetched 上游本次下发的模型数。
	Fetched int `json:"fetched"`
	// Added 新追加进配置的模型（用户原来没有的）。
	Added []string `json:"added"`
	// Kept 用户手填、本次未被上游下发但仍然保留的模型。
	Kept []string `json:"kept"`
	// Total 同步后该供应商的模型总数。
	Total int    `json:"total"`
	Error string `json:"error,omitempty"`
	// Warning 非致命提示（例如合并了多个账号的结果）。
	Warning string `json:"warning,omitempty"`
}

// syncProviderModels 拉一个供应商的目录。
//
// workbuddy 走 /v3/config + 企业端点合并（每个可用账号各拉一次，结果求并集）；
// kind=openai 走标准 /models。其它 kind 暂不支持，返回明确错误而不是静默跳过。
func (s *Server) syncProviderModels(ctx context.Context, p *config.Provider) ([]string, string, error) {
	if p == nil {
		return nil, "", fmt.Errorf("供应商为空")
	}
	switch provider.NormalizeKind(p.Kind) {
	case provider.KindWorkBuddy, "":
		// kind 留空且 id 是 workbuddy 系列时也按 WorkBuddy 处理（历史配置常见）。
		if provider.NormalizeKind(p.Kind) == "" && !isWorkBuddyProvider(p) {
			return nil, "", fmt.Errorf("供应商未配置 kind")
		}
		return s.syncWorkBuddy(ctx, p)
	case provider.KindTrae:
		return s.syncTrae(ctx, p)
	case provider.KindOpenAI:
		base := p.BaseURL
		if strings.TrimSpace(base) == "" {
			base = provider.DefaultBaseURL(provider.KindOpenAI)
		}
		if strings.TrimSpace(base) == "" {
			return nil, "", fmt.Errorf("缺少 baseUrl")
		}
		ids, err := openaicatalog.Fetch(ctx, base, p.APIKey)
		if err != nil {
			return nil, "", err
		}
		return ids, "", nil
	default:
		return nil, "", fmt.Errorf("该 kind 暂不支持目录同步（只支持 openai / workbuddy）")
	}
}

// isWorkBuddyProvider 判断是否 WorkBuddy 供应商（按 kind 或历史 id 约定）。
func isWorkBuddyProvider(p *config.Provider) bool {
	if p == nil {
		return false
	}
	if provider.NormalizeKind(p.Kind) == provider.KindWorkBuddy {
		return true
	}
	return p.ID == "workbuddy" || p.ID == "workbuddy-ai"
}

// syncWorkBuddy 逐个账号拉目录并求并集。
//
// 一个账号失败不影响其它账号；全部失败才报错，并把各自的失败原因拼进错误信息
// ——「同步不到」时用户最想知道的是哪个号挂了、为什么。
func (s *Server) syncWorkBuddy(ctx context.Context, p *config.Provider) ([]string, string, error) {
	accs := make([]*config.Account, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		if a != nil && a.IsEnabled() && a.Key() != "" {
			accs = append(accs, a)
		}
	}
	if len(accs) == 0 {
		return nil, "", fmt.Errorf("没有可用账号")
	}

	origin := strings.TrimSpace(p.BaseURL)
	// 没填 baseUrl 时按 id 兜底，与 handleStatus 里的签到口径保持一致。
	if origin == "" {
		if p.ID == "workbuddy-ai" {
			origin = "https://www.workbuddy.ai"
		} else {
			origin = "https://copilot.tencent.com"
		}
	}

	type seen struct {
		order []string
		set   map[string]bool
	}
	agg := &seen{set: make(map[string]bool, 64)}
	var failures []string
	var mergeNotes []string
	used := 0

	for _, a := range accs {
		acc := &workbuddy.Account{
			UID:    a.UID,
			Token:  a.Key(),
			Domain: a.Domain,
			Realm:  a.Realm,
			// config.Account 目前没有存企业 ID（登录流程也没落盘），
			// 所以这里不填；个人号本来也不需要这个头。
		}
		cr, err := workbuddy.FetchCatalogWithProbes(ctx, origin, acc)
		if err != nil {
			failures = append(failures, fmt.Sprintf("账号 %s: %v", accountLabel(a), err))
			continue
		}
		used++
		// 记录该账号每个站点各自的产出，供面板说明「为什么数量是这么多」
		if cr.Merged {
			perOrigin := map[string]int{}
			for _, pr := range cr.Probes {
				if pr.Error == "" && pr.Kept > 0 {
					perOrigin[pr.Origin] += pr.Kept
				}
			}
			if len(perOrigin) > 1 {
				var parts []string
				for o, n := range perOrigin {
					parts = append(parts, fmt.Sprintf("%s:%d", shortHost(o), n))
				}
				mergeNotes = append(mergeNotes, accountLabel(a)+" 双站合并("+strings.Join(parts, " + ")+")")
			}
		}
		for _, mi := range cr.Models {
			if id := strings.TrimSpace(mi.ID); id != "" && !agg.set[id] {
				agg.set[id] = true
				agg.order = append(agg.order, id)
			}
		}
	}

	if len(agg.order) == 0 {
		return nil, "", fmt.Errorf("所有账号均同步失败: %s", strings.Join(failures, "；"))
	}
	warn := ""
	if len(mergeNotes) > 0 {
		warn = strings.Join(mergeNotes, "；")
	}
	if len(failures) > 0 {
		warn = fmt.Sprintf("%d/%d 个账号失败: %s", len(failures), len(accs), strings.Join(failures, "；"))
	} else if used > 1 {
		warn = fmt.Sprintf("已合并 %d 个账号的目录", used)
	}
	return agg.order, warn, nil
}

// accountLabel 账号的展示名，用于错误信息（不打印凭据）。
func accountLabel(a *config.Account) string {
	if a == nil {
		return "?"
	}
	if strings.TrimSpace(a.Name) != "" {
		return a.Name
	}
	if strings.TrimSpace(a.ID) != "" {
		return a.ID
	}
	return "?"
}

// mergeModels 把上游结果追加进现有列表（只追加，见文件头注释）。
//
// 返回新列表、本次新增的、以及上游本次没下发但保留的用户值。
func mergeModels(current, fetched []string) (merged, added, kept []string) {
	have := make(map[string]bool, len(current))
	merged = make([]string, 0, len(current)+len(fetched))
	for _, m := range current {
		m = strings.TrimSpace(m)
		if m == "" || have[m] {
			continue
		}
		have[m] = true
		merged = append(merged, m)
	}

	fetchedSet := make(map[string]bool, len(fetched))
	added = make([]string, 0, len(fetched))
	for _, m := range fetched {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		fetchedSet[m] = true
		if have[m] {
			continue
		}
		have[m] = true
		merged = append(merged, m)
		added = append(added, m)
	}

	for _, m := range current {
		m = strings.TrimSpace(m)
		if m != "" && !fetchedSet[m] {
			kept = append(kept, m)
		}
	}
	return merged, added, kept
}

// syncTargets 解析要同步哪些供应商。providerId 为空表示全部。
func (s *Server) syncTargets(cfg *config.Config, providerID string) ([]*config.Provider, error) {
	providerID = strings.TrimSpace(providerID)
	if providerID != "" {
		p := cfg.FindProvider(providerID)
		if p == nil {
			return nil, fmt.Errorf("供应商不存在: %s", providerID)
		}
		return []*config.Provider{p}, nil
	}
	out := make([]*config.Provider, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p != nil && p.IsEnabled() {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("配置里没有启用的供应商")
	}
	return out, nil
}

// handleProviderCatalog 只读预览：拉目录但不写配置。
//
//	GET /api/providers/catalog?providerId=xxx
//
// providerId 为空时对全部启用的供应商各拉一次。
func (s *Server) handleProviderCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "只支持 GET"})
		return
	}
	cfg := s.store.Snapshot()
	targets, err := s.syncTargets(cfg, r.URL.Query().Get("providerId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	rows := make([]map[string]any, 0, len(targets))
	for _, p := range targets {
		ctx, cancel := context.WithTimeout(r.Context(), syncTimeout)
		ids, warn, ferr := s.syncProviderModels(ctx, p)
		cancel()

		row := map[string]any{"providerId": p.ID, "kind": p.Kind}
		if ferr != nil {
			row["ok"] = false
			row["error"] = ferr.Error()
			rows = append(rows, row)
			continue
		}
		// 预览态也给出「同步后会新增什么」，用户据此决定要不要真同步。
		_, added, kept := mergeModels(p.Models, ids)
		row["ok"] = true
		row["fetched"] = len(ids)
		row["wouldAdd"] = added
		row["kept"] = kept
		row["current"] = len(p.Models)
		row["total"] = len(p.Models) + len(added)
		row["models"] = ids
		if warn != "" {
			row["warning"] = warn
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dryRun": true, "providers": rows})
}

// handleProviderSync 同步模型目录并写回配置。
//
//	POST /api/providers/sync   {"providerId":"可选，为空则全部"}
//
// 全部失败时返回 502；部分失败返回 200 但每行带 error，便于用户逐条排查。
func (s *Server) handleProviderSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "只支持 POST"})
		return
	}
	var opt struct {
		ProviderID string `json:"providerId"`
	}
	// body 可以为空（表示同步全部），所以解析失败不当错误处理。
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&opt)
	}
	if opt.ProviderID == "" {
		opt.ProviderID = r.URL.Query().Get("providerId")
	}

	cfg := s.store.Snapshot()
	targets, err := s.syncTargets(cfg, opt.ProviderID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	rows := make([]syncRow, 0, len(targets))
	anyOK := false
	changed := false

	for _, p := range targets {
		row := syncRow{ProviderID: p.ID, Kind: p.Kind, Added: []string{}, Kept: []string{}, Total: len(p.Models)}

		ctx, cancel := context.WithTimeout(r.Context(), syncTimeout)
		ids, warn, ferr := s.syncProviderModels(ctx, p)
		cancel()

		if ferr != nil {
			row.Error = ferr.Error()
			rows = append(rows, row)
			continue
		}

		merged, added, kept := mergeModels(p.Models, ids)
		row.OK = true
		row.Fetched = len(ids)
		row.Added = added
		row.Kept = kept
		row.Total = len(merged)
		row.Warning = warn
		anyOK = true

		if len(added) > 0 {
			p.Models = merged
			changed = true
		}
		rows = append(rows, row)
	}

	if changed {
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "error": "保存配置失败: " + err.Error(), "providers": rows,
			})
			return
		}
	}

	code := http.StatusOK
	if !anyOK {
		code = http.StatusBadGateway
	}
	writeJSON(w, code, map[string]any{
		"ok":         anyOK,
		"changed":    changed,
		"providers":  rows,
		"syncedAt":   time.Now().Unix(),
		"modelTotal": len(provider.ModelList(cfg)),
	})
}

// StartAutoSyncModels 在启动时对 autoSyncModels=true 的供应商同步一次目录。
//
// 设计取舍：不阻塞启动（ListenAndServe 之前不做网络请求），失败了只记日志——
// 启动期上游不可达是常态，不该让网关起不来。
func (s *Server) StartAutoSyncModels(ctx context.Context) {
	cfg := s.store.Snapshot()
	var targets []*config.Provider
	for _, p := range cfg.Providers {
		if p != nil && p.IsEnabled() && p.AutoSyncModels {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return
	}

	for _, p := range targets {
		ctx, cancel := context.WithTimeout(ctx, syncTimeout)
		ids, warn, err := s.syncProviderModels(ctx, p)
		cancel()
		if err != nil {
			log.Printf("自动同步模型目录失败 [%s]: %v", p.ID, err)
			continue
		}
		if warn != "" {
			log.Printf("自动同步模型目录 [%s]: %s", p.ID, warn)
		}

		// 重新取快照再改：同步期间面板可能改过配置，直接写旧副本会把改动冲掉。
		fresh := s.store.Snapshot()
		fp := fresh.FindProvider(p.ID)
		if fp == nil {
			continue
		}
		merged, added, _ := mergeModels(fp.Models, ids)
		if len(added) == 0 {
			log.Printf("自动同步模型目录 [%s]: 无新增（上游 %d 个，配置 %d 个）", p.ID, len(ids), len(fp.Models))
			continue
		}
		fp.Models = merged
		if err := s.store.Save(fresh); err != nil {
			log.Printf("自动同步模型目录 [%s]: 保存失败: %v", p.ID, err)
			continue
		}
		log.Printf("自动同步模型目录 [%s]: 新增 %d 个，共 %d 个", p.ID, len(added), len(merged))
	}
}

// shortHost 把 URL 缩成域名，用于提示文案。
func shortHost(o string) string {
	o = strings.TrimPrefix(o, "https://")
	o = strings.TrimPrefix(o, "http://")
	o = strings.TrimSuffix(o, "/")
	return o
}

// syncTrae 拉 Trae CN 的模型目录。
//
// 之前 Trae 的模型是硬编码占位值（claude-3.5-sonnet / gpt-4o），
// Trae CN 根本不提供这两家模型，调不通。现在从
// /api/ide/v1/batch_get_detail_param 真拉。
func (s *Server) syncTrae(ctx context.Context, p *config.Provider) ([]string, string, error) {
	var acc *config.Account
	for _, a := range p.Accounts {
		if a != nil && a.IsEnabled() && a.Key() != "" {
			acc = a
			break
		}
	}
	if acc == nil {
		return nil, "", fmt.Errorf("没有可用 Trae 账号（需要先登录）")
	}

	ta := &trae.Account{
		UID: acc.UID, Token: acc.Key(),
		DeviceID: acc.DeviceID, MachineID: acc.MachineID,
		PublicKey: acc.PublicKey, PrivateKey: acc.PrivateKey,
	}
	entries, err := trae.FetchModels(ctx, ta)
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name != "" {
			names = append(names, e.Name)
		}
	}
	if len(names) == 0 {
		return nil, "", fmt.Errorf("上游没有返回模型")
	}
	return names, fmt.Sprintf("拉到 %d 个模型（含 function 分组）", len(names)), nil
}
