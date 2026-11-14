// catalog.go —— WorkBuddy 模型目录拉取。
//
// 为什么要动态拉：配置里手写的模型名（glm-5.3 / deepseek-v4.1-flash / hy3 …）
// 只是上游实际下发目录里的一小撮，用户看不到也调不到其余几十个模型。
//
// 三个端点的实测行为（CN 域 copilot.tencent.com，真实登录态 token，2026-11 实测）：
//
//	GET {origin}/v3/config                            → code=0，data.models 全量 + 能力字段
//	GET {origin}/console/enterprises/personal/models  → code=0，data.agents[cli].models
//	                                                    是「精选列表」，data.models 是更大全集
//	GET {origin}/v1/models                            → 404 Route Not Found（不存在）
//
// global 域（www.workbuddy.ai）只提供 /v3/config，企业端点是 500 / 404，属正常。
//
// 合并顺序：/v3/config 为主（字段最全，带 reasoning.supportedEfforts），
// 企业端点补它没有的 id；同 id 以 /v3/config 为准。与 wb2api 的
// mergeModelInfos（internal/upstream/client.go）口径一致。
package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// 目录端点（相对 origin）。
const (
	PathCatalogV3         = "/v3/config"
	PathCatalogEnterprise = "/console/enterprises/personal/models"
)

// 不同 UA 下发的模型集合**不一样**，实测 2026-11-14：
//
//	CLI UA  "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0" → 59 个模型
//	IDE UA  "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"        → 24 个模型
//
// 所以两套都探一遍取并集，谁多要谁。UA 里必须能解析出 CodeBuddy 版本号，
// 否则 /v3/config 返回 400 code=12403。
const (
	catalogUACLI = "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0"
	catalogUAIDE = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"
)

// ModelInfo 是一条模型目录条目。
//
// 只保留网关用得到的字段：能不能看图、支不支持推理、档位有哪些。
// 上游还有 credits / temperature / iconId 等展示字段，这里不解析（不编造，也不用）。
type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// SupportsImages 多模态图片输入能力。
	SupportsImages bool `json:"supportsImages"`
	// SupportsReasoning 是否支持推理（思考）。
	SupportsReasoning bool `json:"supportsReasoning"`
	// SupportedEfforts 可用的思考档位（low/medium/high/xhigh/max），空表示上游未声明。
	SupportedEfforts []string `json:"supportedEfforts,omitempty"`
	// DefaultEffort 默认档位：优先 reasoning.defaultEffort，缺省回落 reasoning.effort。
	DefaultEffort string `json:"defaultEffort,omitempty"`
	// ContextWindow / MaxTokens 对应 maxInputTokens / maxOutputTokens（0 = 上游未下发）。
	ContextWindow int64 `json:"contextWindow,omitempty"`
	MaxTokens     int64 `json:"maxTokens,omitempty"`
	// OnlyReasoning 纯推理模型（思考不可关）。
	OnlyReasoning bool `json:"onlyReasoning,omitempty"`
	// SupportsToolCall 是否支持工具调用。
	SupportsToolCall bool `json:"supportsToolCall,omitempty"`
	// Tags 上游标签（含 badge: 前缀的营销标签与 text-to-image 这类生成类标签）。
	Tags []string `json:"tags,omitempty"`
}

// Account 是目录探测所需的账号信息。
//
// 与 config.Account 解耦：本包不认识 config 包（同 Customer 的理由）。
type Account struct {
	// UID 账号 uid，对应 X-User-Id。
	UID string
	// Token 访问令牌（Authorization: Bearer）。
	Token string
	// Domain X-Domain 头的值，一般就是登录时 /login/account 返回的 domain。
	Domain string
	// EnterpriseID 非空时补 X-Enterprise-Id / X-Tenant-Id。
	EnterpriseID string
	// Realm cn / global，决定 Accept-Language 与默认域名。
	Realm string
}

// IsGlobal 判断是否国际版账号。
func (a *Account) IsGlobal() bool {
	if a == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.Realm), "global") ||
		strings.Contains(strings.ToLower(a.Domain), "workbuddy.ai")
}

// OriginFor 返回账号对应的域名（不带 /v1）。
//
// 显式传入的 origin 优先（自建反代也能用）；为空时按 realm 兜底。
func OriginFor(origin string, acc *Account) string {
	base := strings.TrimSpace(origin)
	if base == "" {
		if acc.IsGlobal() {
			base = "https://www.workbuddy.ai"
		} else {
			base = "https://copilot.tencent.com"
		}
	}
	base = strings.TrimRight(base, "/")
	// 配置里 baseUrl 惯例带 /v1（见 config.template.json），目录路径在根上。
	return strings.TrimSuffix(base, "/v1")
}

// expectedHost 返回该账号的官方域名。
func expectedHost(acc *Account) string {
	if acc.IsGlobal() {
		return "www.workbuddy.ai"
	}
	return "copilot.tencent.com"
}

// ValidateCatalogURL 校验目录 URL 合法（协议、主机、路径）。
//
// 这条校验不只是洁癖：请求里带着 Bearer token，origin 一旦被配成恶意地址，
// token 就随请求一起泄露了。
func ValidateCatalogURL(raw string, acc *Account) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("目录地址非法: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return fmt.Errorf("目录地址协议非法: %s", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("目录地址不允许带用户信息")
	}
	if u.Host == "" {
		return fmt.Errorf("目录地址缺少主机名")
	}
	return CheckCatalogPath(u.Path)
}

// CheckCatalogPath 校验路径必须是已知的目录端点之一。
func CheckCatalogPath(path string) error {
	switch path {
	case PathCatalogV3, PathCatalogEnterprise:
		return nil
	}
	return fmt.Errorf("不是已知的模型目录端点: %s", path)
}

// catalogEntry 是上游模型对象的解析形态（/v3/config 与 /console 同构）。
//
// ModelID / Model 是宽松回退键：个别信封用它们代替 id。
type catalogEntry struct {
	ID              string   `json:"id"`
	ModelID         string   `json:"modelId"`
	Model           string   `json:"model"`
	Name            string   `json:"name"`
	Description     string   `json:"descriptionZh"`
	Tags            []string `json:"tags"`
	Disabled        bool     `json:"disabled"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsReason  bool     `json:"supportsReasoning"`
	SupportsTool    bool     `json:"supportsToolCall"`
	OnlyReasoning   bool     `json:"onlyReasoning"`
	Reasoning       struct {
		Effort             string   `json:"effort"`
		DefaultEffort      string   `json:"defaultEffort"`
		SupportedEfforts   []string `json:"supportedEfforts"`
		CanDisableThinking bool     `json:"canDisableThinking"`
	} `json:"reasoning"`
}

// id 取模型标识，宽松回退三个键。
func (e catalogEntry) id() string {
	for _, cand := range []string{e.ID, e.ModelID, e.Model} {
		if v := strings.TrimSpace(cand); v != "" {
			return v
		}
	}
	return ""
}

// modelInfo 把解析条目映射成 ModelInfo（两个端点共用，避免映射漂移）。
func (e catalogEntry) modelInfo() ModelInfo {
	def := strings.TrimSpace(e.Reasoning.DefaultEffort)
	if def == "" {
		def = strings.TrimSpace(e.Reasoning.Effort)
	}
	return ModelInfo{
		ID:                e.id(),
		Name:              e.Name,
		Description:       e.Description,
		SupportsImages:    e.SupportsImages,
		SupportsReasoning: e.SupportsReason,
		SupportedEfforts:  e.Reasoning.SupportedEfforts,
		DefaultEffort:     def,
		ContextWindow:     e.MaxInputTokens,
		MaxTokens:         e.MaxOutputTokens,
		OnlyReasoning:     e.OnlyReasoning,
		SupportsToolCall:  e.SupportsTool,
		Tags:              e.Tags,
	}
}

// nonChatModel 判定是否「非对话模型」，这类条目要从目录里剔除。
//
// 规则照抄 wb2api 的 nonChatModel（来源 harness buddy.ts:547-555）：选了它们上游会
// 报 code=11102，留在列表里只会让客户端选到坏模型。三类：
//   - id 前缀 nes- / completion- / codewise-：补全、代码改写等专用模型；
//   - maxOutputTokens ≤ 256：tiny 输出，不是对话模型；
//   - tags 含生成类标签：图片/视频生成走各自专用端点。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	low := strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return true
		}
	}
	return false
}

// FetchCatalog 拉一个账号的模型目录。
//
// 两个端点并发探测：/v3/config 为主（字段最全），企业端点补缺。任一成功即返回，
// 都失败才返回错误——两路各自独立容错，一路挂了不拖累另一路。
func FetchCatalog(ctx context.Context, origin string, acc *Account) ([]ModelInfo, error) {
	if acc == nil {
		return nil, fmt.Errorf("账号为空")
	}
	if strings.TrimSpace(acc.Token) == "" {
		return nil, fmt.Errorf("账号缺少访问令牌")
	}
	base := OriginFor(origin, acc)

	type result struct {
		infos []ModelInfo
		err   error
	}
	v3Ch := make(chan result, 1)
	entCh := make(chan result, 1)
	go func() {
		infos, err := probeV3(ctx, base, acc)
		v3Ch <- result{infos, err}
	}()
	go func() {
		infos, err := probeEnterprise(ctx, base, acc)
		entCh <- result{infos, err}
	}()
	v3Res, entRes := <-v3Ch, <-entCh

	if v3Res.err != nil && entRes.err != nil {
		return nil, fmt.Errorf("两个目录端点都失败: v3/config: %v; 企业端点: %v", v3Res.err, entRes.err)
	}
	merged := mergeModelInfos(v3Res.infos, entRes.infos)
	if len(merged) == 0 {
		return nil, fmt.Errorf("目录为空（两路均无可用的对话模型）")
	}
	return merged, nil
}

// probeV3 探测 /v3/config。
func probeV3(ctx context.Context, base string, acc *Account) ([]ModelInfo, error) {
	// 先用 CLI UA（模型更全），失败再退回 IDE UA
	raw, err := catalogGet(ctx, base+PathCatalogV3, acc, catalogUACLI)
	if err != nil {
		raw, err = catalogGet(ctx, base+PathCatalogV3, acc, catalogUAIDE)
	}
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Models []catalogEntry `json:"models"`
			// 试用模型横幅：上游把「N 天免费试用」的模型只放这里，data.models 里没有。
			ProductFeaturesConfig struct {
				ModelTrialBanner struct {
					Banners []struct {
						ModelID       string `json:"modelId"`
						TargetModelID string `json:"targetModelId"`
					} `json:"banners"`
				} `json:"ModelTrialBanner"`
			} `json:"productFeaturesConfig"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("v3/config 响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("v3/config 返回 code=%d msg=%s", env.Code, env.Msg)
	}

	byID := make(map[string]ModelInfo, len(env.Data.Models))
	out := make([]ModelInfo, 0, len(env.Data.Models))
	for _, e := range env.Data.Models {
		mi := e.modelInfo()
		if mi.ID == "" || e.Disabled {
			continue
		}
		if nonChatModel(mi.ID, e.MaxOutputTokens, e.Tags) {
			continue
		}
		if _, dup := byID[mi.ID]; dup {
			continue
		}
		byID[mi.ID] = mi
		out = append(out, mi)
	}

	// 试用横幅里的模型实际可调用，按目标模型继承能力，只换 id。
	for _, b := range env.Data.ProductFeaturesConfig.ModelTrialBanner.Banners {
		id := strings.TrimSpace(b.ModelID)
		if id == "" {
			continue
		}
		if _, exists := byID[id]; exists {
			continue
		}
		mi := ModelInfo{ID: id, Name: id}
		if tgt := strings.TrimSpace(b.TargetModelID); tgt != "" {
			if parent, ok := byID[tgt]; ok {
				mi = parent
				mi.ID = id
			}
		}
		byID[id] = mi
		out = append(out, mi)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config 没有可用的对话模型")
	}
	return out, nil
}

// probeEnterprise 探测企业模型端点。
//
// 口径：data.agents 里 name=cli 的模型列表是「这个账号能用什么」，data.models
// 是带元数据的全集，两边按 id 求交，再过掉非对话模型。没有 cli agent 时退化成
// 直接用 data.models（部分账号就是这样）。
func probeEnterprise(ctx context.Context, base string, acc *Account) ([]ModelInfo, error) {
	// 企业端点吃 WorkBuddy 桌面端 UA（与 tasks.go 一致），不是 IDE UA。
	ua := "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0"
	if acc.IsGlobal() {
		ua = "WorkBuddy/5.9.3 WorkBuddy AI/5.9.3 CLI/2.165.0"
	}
	raw, err := catalogGet(ctx, base+PathCatalogEnterprise, acc, ua)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
			Models []catalogEntry `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("企业端点响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("企业端点返回 code=%d msg=%s", env.Code, env.Msg)
	}

	meta := make(map[string]catalogEntry, len(env.Data.Models))
	for _, e := range env.Data.Models {
		if id := e.id(); id != "" && !e.Disabled {
			meta[id] = e
		}
	}

	var picked []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			picked = ag.Models
			break
		}
	}
	if len(picked) == 0 {
		for _, e := range env.Data.Models {
			if id := e.id(); id != "" && !e.Disabled {
				picked = append(picked, id)
			}
		}
	}

	seen := make(map[string]bool, len(picked))
	out := make([]ModelInfo, 0, len(picked))
	for _, id := range picked {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		e, ok := meta[id]
		if !ok {
			// cli 列表里有、元数据里没有：仍然收下（上游说这个账号能用它），
			// 只是能力字段缺省，不编造。
			seen[id] = true
			out = append(out, ModelInfo{ID: id, Name: id})
			continue
		}
		if nonChatModel(id, e.MaxOutputTokens, e.Tags) {
			continue
		}
		seen[id] = true
		out = append(out, e.modelInfo())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("企业端点没有可用的对话模型")
	}
	return out, nil
}

// catalogGet 发一次目录 GET 请求。鉴权头与签到 / 任务中心一致
// （见 tasks.go 的 Customer.setHeaders），额外补 /v3/config 认的身份头。
func catalogGet(ctx context.Context, endpoint string, acc *Account, ua string) ([]byte, error) {
	if err := ValidateCatalogURL(endpoint, acc); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(acc.Token))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("X-Product", "SaaS")
	if acc.IsGlobal() {
		req.Header.Set("Accept-Language", "en-US")
	} else {
		req.Header.Set("Accept-Language", "zh-CN")
	}

	host := expectedHost(acc)
	if d := strings.TrimSpace(acc.Domain); d != "" {
		d = strings.TrimPrefix(d, "https://")
		d = strings.TrimPrefix(d, "http://")
		if d = strings.TrimSuffix(d, "/"); d != "" {
			host = d
		}
	}
	req.Header.Set("X-Domain", host)
	if uid := strings.TrimSpace(acc.UID); uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	if ent := strings.TrimSpace(acc.EnterpriseID); ent != "" {
		req.Header.Set("X-Enterprise-Id", ent)
		req.Header.Set("X-Tenant-Id", ent)
	}

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if rerr != nil {
		return nil, fmt.Errorf("读取响应失败: %w", rerr)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("授权状态无效（HTTP %d）", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateText(string(raw), 120))
	}
	return raw, nil
}

// mergeModelInfos 合并两路目录：primary 为主（同 id 以 primary 的字段为准），
// secondary 只补 primary 缺失的 id。输出顺序稳定：primary 原序在前，补充项在后。
func mergeModelInfos(primary, secondary []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(primary)+len(secondary))
	seen := make(map[string]bool, len(primary)+len(secondary))
	for _, list := range [][]ModelInfo{primary, secondary} {
		for _, mi := range list {
			if mi.ID == "" || seen[mi.ID] {
				continue
			}
			seen[mi.ID] = true
			out = append(out, mi)
		}
	}
	return out
}

// truncateText 截断错误摘要，避免把整页 HTML 塞进日志。
func truncateText(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
