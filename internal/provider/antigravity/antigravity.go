// Package antigravity 适配 Google Antigravity（cloudcode-pa / Code Assist）的私有协议。
//
// 全部抄自 All-in 的 AntigravityAuth.java 与 AntigravityClient.java。调用链路：
//
//	登录：GET  https://accounts.google.com/o/oauth2/v2/auth   （OAuth 2.0 + PKCE，回环回调）
//	      POST https://oauth2.googleapis.com/token            （code 换 token / refresh）
//	      GET  https://www.googleapis.com/oauth2/v2/userinfo    （补 email）
//	      POST {cloudcode-pa}/v1internal:loadCodeAssist         （问账号的项目与 tier）
//	      POST {cloudcode-pa}/v1internal:onboardUser            （新账号领项目，LRO）
//	聊天：POST {daily-cloudcode-pa}/v1internal:streamGenerateContent?alt=sse
//	      POST {cloudcode-pa}/v1internal:fetchAvailableModels   （模型清单 + 配额）
//
// 四个必须照抄的点：
//
//  1. 聊天/建模走 daily-cloudcode-pa，取项目走 cloudcode-pa（上游就是这么分的）。
//     两个域是同一个后端，谁通就用谁；但只有「链路根本没建起来」才换域。
//  2. 登录取的是 Antigravity 自己的 OAuth client（installed app），回调必须是
//     http://localhost:1456/oauth-callback。凭据 = access_token + refresh_token
//     （Google refresh token 不过期，只需要刷 access）。
//  3. 请求要带 project、userAgent=antigravity、requestType=agent、
//     requestId=agent-<hex>、request.sessionId = "-" + sha256(首条用户消息) 的低 63 位。
//  4. 对「Claude Code / Claude Agent SDK / Codex GPT-5 / Hermes 旧人设」这类 system
//     prompt，Antigravity 会回假 429（不是额度）：默认在命中的片段第一个字符后插一个
//     U+200B 打散。见 CloakSystem。
//
// 传输层注意：cloudcode-pa 对 ALPN 协商到 h2 的连接直接 reset（实测一律失败，
// HTTP/1.1 才有响应），所以本包的 HTTPClient 关掉了 HTTP/2。
package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 协议常量。全部对应 AntigravityAuth.java / AntigravityClient.java 里硬编码的值。
const (
	// ProviderID 是供应商 id（config 里的 "antigravity"）。
	ProviderID = "antigravity"

	// ClientID 是 Antigravity 自己的 OAuth client id（installed app）。
	ClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	// ClientSecret 同上；installed app 的 secret 是公开的。
	ClientSecret = "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"

	// AuthorizeURL Google OAuth 授权页（/o/oauth2/v2/auth）。
	AuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	// TokenURL code 换 token 与 refresh 都用这一个。
	TokenURL = "https://oauth2.googleapis.com/token"
	// UserInfoURL 取 email。
	UserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"

	// ReqBase 聊天与 onboard 域。
	ReqBase = "https://daily-cloudcode-pa.googleapis.com"
	// LoadBase 取项目域。
	LoadBase = "https://cloudcode-pa.googleapis.com"

	// StreamPath 聊天端点（SSE）。query 里的 alt=sse 不能省。
	StreamPath = "/v1internal:streamGenerateContent?alt=sse"
	// PathLoadCodeAssist 问项目与 tier。
	PathLoadCodeAssist = "/v1internal:loadCodeAssist"
	// PathOnboardUser 新账号领项目（LRO）。
	PathOnboardUser = "/v1internal:onboardUser"
	// PathFetchModels 模型清单（含配额）。
	PathFetchModels = "/v1internal:fetchAvailableModels"

	// CallbackPort Google 只接受 localhost 上的这个端口。
	CallbackPort = 1456
	// CallbackPath 回调路径。
	CallbackPath = "/oauth-callback"
	// CallbackURL 拼好的回调地址。
	CallbackURL = "http://localhost:1456/oauth-callback"

	// Scopes 授权范围（空格分隔）。
	Scopes = "https://www.googleapis.com/auth/cloud-platform " +
		"https://www.googleapis.com/auth/userinfo.email " +
		"https://www.googleapis.com/auth/userinfo.profile " +
		"https://www.googleapis.com/auth/cclog " +
		"https://www.googleapis.com/auth/experimentsandconfigs"

	// AppVersion 客户端版本（也进 onboard 的 ide_version）。
	AppVersion = "2.9.1"
	// UserAgent 客户端 UA，Code Assist 认这个串。
	UserAgent = "antigravity/hub/" + AppVersion + " linux/x64"
	// GoogleAPIUserAgent 取项目 / onboard 时额外拼的尾巴。
	GoogleAPIUserAgent = UserAgent + " google-api-nodejs-client/10.3.0"

	// EnvelopeUserAgent 信封里的 userAgent 字段（不是 HTTP 头）。
	EnvelopeUserAgent = "antigravity"
	// RequestType 信封里的 requestType。
	RequestType = "agent"
	// IdeType loadCodeAssist 的 metadata.ideType。
	IdeType = "ANTIGRAVITY"
	// OnboardTier 新账号领项目的档位。
	OnboardTier = "free-tier"

	// ZeroWidth 用来打散会被拒的 system prompt 片段。
	ZeroWidth = "\u200b"
)

// RefreshMargin access_token 剩余不足这个时长就提前刷（Java 侧是 3 分钟）。
const RefreshMargin = 3 * time.Minute

// Model 是模型清单的一项。
type Model struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Context   int     `json:"context,omitempty"`
	Output    int     `json:"output,omitempty"`
	Remaining float64 `json:"remaining,omitempty"`
	Reset     string  `json:"reset,omitempty"`
}

// DefaultModels 是 Antigravity 不说有哪些模型时的兜底（upstream antigravityModels）。
var DefaultModels = []Model{
	{ID: "gemini-pro-agent", Name: "Gemini 3.1 Pro (High)"},
	{ID: "gemini-3.1-pro-low", Name: "Gemini 3.1 Pro (Low)"},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash"},
	{ID: "gemini-3.1-flash-lite", Name: "Gemini 3.1 Flash Lite"},
	{ID: "claude-opus-4-6-thinking", Name: "Claude Opus 4.6 (Thinking)"},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
	{ID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B (Medium)"},
}

// HiddenModels 是清单里列出来但不用于聊天的 id。
var HiddenModels = []string{
	"chat_20706", "chat_23310", "tab_flash_lite_preview", "tab_jump_flash_lite_preview",
	"gemini-2.5-flash-thinking", "gemini-2.5-pro",
}

// TurnedAway 是 Antigravity 据以回「假 429」的 system prompt 片段（全小写匹配）。
var TurnedAway = []string{
	"x-anthropic-billing-header:",
	"built on anthropic's claude agent sdk",
	"you are codex, an agent based on gpt-5",
	"you are codex, a coding agent based on gpt-5",
	"hermes agent, an intelligent ai assistant created by nous research",
}

// HTTPClient 是所有上游请求共用的客户端。
//
// ForceAttemptHTTP2=false 是硬性要求：cloudcode-pa 一旦协商到 h2 就直接 reset。
// 流式响应不能设总超时，靠 ctx 控制。
var HTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 3 * time.Second,
	},
}

// ErrSignInRequired 表示 refresh_token 已不可用，只能让用户重新登录。
var ErrSignInRequired = errors.New("antigravity: 需要重新登录（refresh_token 不可用或被撤销）")

// ErrInsecureURL 只接受 https 的上游地址（与原实现的校验一致）。
var ErrInsecureURL = errors.New("antigravity: 上游地址必须是 https")

// Account 是一个 Antigravity（Google 账号）凭据。
//
// Expires 是毫秒时间戳；Access 与 Token 同值（兼容只写 access 的配置）。
type Account struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Type     string `json:"type,omitempty"`
	Email    string `json:"email,omitempty"`
	Access   string `json:"access,omitempty"`
	Token    string `json:"token,omitempty"`
	Refresh  string `json:"refresh,omitempty"`
	Expires  int64  `json:"expires,omitempty"`
	Project  string `json:"project,omitempty"`
	Tier     string `json:"tier,omitempty"`
	PaidTier string `json:"paidTier,omitempty"`
	Plan     string `json:"plan,omitempty"`
	AddedAt  int64  `json:"addedAt,omitempty"`
	Enabled  bool   `json:"enabled,omitempty"`
}

// Bearer 返回可用的 access_token（Access 优先，回退 Token）。
func (a *Account) Bearer() string {
	if a == nil {
		return ""
	}
	if a.Access != "" {
		return a.Access
	}
	return a.Token
}

// Headers 返回聊天 / Code Assist 接口需要的头。
//
// 关键只有两个：Bearer 凭据、User-Agent（Code Assist 认 antigravity/hub/<ver> linux/x64）。
func Headers(acc *Account) map[string]string {
	token := ""
	if acc != nil {
		token = acc.Bearer()
	}
	return map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "text/event-stream",
		"Authorization": "Bearer " + token,
		"User-Agent":    UserAgent,
	}
}

// Flow 是一次进行中的登录（PKCE）。
//
// Google 这条链没有设备码轮询：授权完成后回调打到本机 1456 端口（或用户手动把
// 回调链接/ code 贴回来）。所以本包不提供 PollLogin，只有 StartLogin / WaitLogin /
// SubmitCallback 三个入口。
type Flow struct {
	FlowID    string
	State     string
	Verifier  string
	Challenge string
	Redirect  string
	AuthURL   string
	CreatedAt time.Time
}

// StartLogin 生成授权链接（PKCE + state，回调固定 localhost:1456/oauth-callback）。
//
// 只做本地计算，不发网络请求 —— 用户拿去浏览器打开即可。
func StartLogin() (*Flow, error) {
	verifier := randomHex(32)
	sum := sha256.Sum256([]byte(verifier))
	f := &Flow{
		FlowID:    randomHex(8),
		State:     randomHex(16),
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		Redirect:  CallbackURL,
		CreatedAt: time.Now(),
	}
	q := url.Values{}
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", f.Redirect)
	q.Set("response_type", "code")
	q.Set("scope", Scopes)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	q.Set("state", f.State)
	q.Set("code_challenge", f.Challenge)
	q.Set("code_challenge_method", "S256")
	f.AuthURL = AuthorizeURL + "?" + q.Encode()
	return f, nil
}

// SubmitCallback 把「用户贴回来的东西」变成 code。
//
// 允许两种输入：完整回调链接（从中取 code 与 state），或裸 code。
func SubmitCallback(input string) (code, state string, err error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", "", errors.New("antigravity: 请输入 code 或回调链接")
	}
	if !strings.HasPrefix(s, "http") {
		return s, "", nil
	}
	// 有的浏览器把 query 放在 # 之后，两种都认。
	if i := strings.Index(s, "#"); i >= 0 && strings.Contains(s[i:], "code=") {
		s = s[:i] + "?" + s[i+1:]
	}
	u, perr := url.Parse(s)
	if perr != nil {
		return "", "", fmt.Errorf("antigravity: 回调链接解析失败: %w", perr)
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return "", "", fmt.Errorf("antigravity: 授权失败（%s）", e)
	}
	if c := q.Get("code"); c != "" {
		return c, q.Get("state"), nil
	}
	return "", "", errors.New("antigravity: 回调链接里没有 code")
}

// ExchangeCode 用授权 code 换 token，并补齐 email、项目、tier。
//
// 项目/档位拿不到不算登录失败：token 已经在手，只是首次请求前还要再跑一次
// EnsureProject（对齐 Java 的 try/catch 语义）。
func ExchangeCode(ctx context.Context, acc *Account, code, verifier, redirect string) error {
	if acc == nil {
		return errors.New("antigravity: account 不能为空")
	}
	if code == "" {
		return errors.New("antigravity: code 不能为空")
	}
	if redirect == "" {
		redirect = CallbackURL
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", ClientID)
	form.Set("client_secret", ClientSecret)
	form.Set("code_verifier", verifier)

	var tok tokenResponse
	if err := postForm(ctx, TokenURL, form, &tok); err != nil {
		return err
	}
	if tok.AccessToken == "" {
		return googleTokenError(tok)
	}
	expiresIn := tok.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	acc.Type = "google"
	acc.Access = tok.AccessToken
	acc.Token = tok.AccessToken
	acc.Refresh = tok.RefreshToken
	acc.Expires = time.Now().UnixMilli() + expiresIn*1000
	acc.AddedAt = time.Now().UnixMilli()
	acc.Enabled = true

	if email, err := WhoAmI(ctx, acc.Access); err == nil {
		acc.Email = email
		if acc.Name == "" {
			acc.Name = email
		}
	}

	load, err := LoadCodeAssist(ctx, acc.Access, "")
	if err != nil {
		return nil // token 有效，只是这次没问到项目
	}
	acc.Project = ProjectOf(load)
	acc.Tier = TierOf(load)
	acc.PaidTier = PaidTierOf(load)
	if p := PlanOf(load); p != "" {
		acc.Plan = p
	}
	if acc.Project == "" {
		if p, oerr := Onboard(ctx, acc.Access); oerr == nil {
			acc.Project = p
		}
	}
	return nil
}

// WaitLogin 在本机 localhost:1456/oauth-callback 等回调，收到 code 后直接换 token。
//
// 端口被占用或 ctx 超时都会返回错误；不想开端口就用 SubmitCallback + ExchangeCode
// （对应 Java 侧 /api/auth/antigravity/submit-code 那条手动粘贴通道）。
func WaitLogin(ctx context.Context, f *Flow) (*Account, error) {
	if f == nil {
		return nil, errors.New("antigravity: 登录流为空")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", CallbackPort))
	if err != nil {
		return nil, fmt.Errorf("antigravity: 回调端口 %d 不可用: %w", CallbackPort, err)
	}
	defer ln.Close()

	type result struct {
		code string
		err  error
	}
	ch := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != CallbackPath {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if e := q.Get("error"); e != "" {
			_, _ = io.WriteString(w, "授权失败，可以关闭此页。")
			ch <- result{err: fmt.Errorf("antigravity: 你在 Google 授权页点了拒绝（%s）", e)}
			return
		}
		if st := q.Get("state"); st != "" && st != f.State {
			_, _ = io.WriteString(w, "state 不匹配，请重新登录。")
			ch <- result{err: errors.New("antigravity: 回调 state 与请求不一致")}
			return
		}
		_, _ = io.WriteString(w, "授权完成，可以关闭此页并回到应用。")
		ch <- result{code: q.Get("code")}
	})}

	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if r.code == "" {
			return nil, errors.New("antigravity: 回调里没有 code")
		}
		acc := &Account{}
		if err := ExchangeCode(ctx, acc, r.code, f.Verifier, f.Redirect); err != nil {
			return nil, err
		}
		return acc, nil
	}
}

// ===================== token 刷新 =====================

// NeedsRefresh 判断 access_token 是否该刷了（缺失，或剩余不足 RefreshMargin）。
func NeedsRefresh(acc *Account, now time.Time) bool {
	if acc == nil || acc.Bearer() == "" {
		return true
	}
	if acc.Expires <= 0 {
		return false // 上游没给过期时间，只能等 401
	}
	return acc.Expires-now.UnixMilli() < RefreshMargin.Milliseconds()
}

// RefreshToken 用 refresh_token 换新的 access_token。
//
// Google 的 refresh_token 通常不轮换，只有上游真给了新值才覆盖。
// refresh_token 缺失或被撤销（invalid_grant）返回 ErrSignInRequired，只能重新登录。
func RefreshToken(ctx context.Context, acc *Account) error {
	if acc == nil || acc.Refresh == "" {
		return ErrSignInRequired
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", acc.Refresh)
	form.Set("client_id", ClientID)
	form.Set("client_secret", ClientSecret)

	var tok tokenResponse
	if err := postForm(ctx, TokenURL, form, &tok); err != nil {
		return err
	}
	if tok.AccessToken == "" {
		if tok.Error == "invalid_grant" {
			return ErrSignInRequired
		}
		return googleTokenError(tok)
	}
	expiresIn := tok.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	acc.Access = tok.AccessToken
	acc.Token = tok.AccessToken
	acc.Expires = time.Now().UnixMilli() + expiresIn*1000
	if tok.RefreshToken != "" {
		acc.Refresh = tok.RefreshToken
	}
	return nil
}

// EnsureToken 保证 access_token 至少还有 RefreshMargin，必要时刷新。
func EnsureToken(ctx context.Context, acc *Account) (string, error) {
	if acc == nil {
		return "", ErrSignInRequired
	}
	if NeedsRefresh(acc, time.Now()) {
		if err := RefreshToken(ctx, acc); err != nil {
			return "", err
		}
	}
	tok := acc.Bearer()
	if tok == "" {
		return "", errors.New("antigravity: 账号没有可用的 access_token")
	}
	return tok, nil
}

// ===================== Code Assist 调用 =====================

// WhoAmI 用 userinfo 取 email。
func WhoAmI(ctx context.Context, access string) (string, error) {
	body, err := doJSON(ctx, http.MethodGet, UserInfoURL, access, nil, UserAgent)
	if err != nil {
		return "", err
	}
	var j struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return "", fmt.Errorf("antigravity: userinfo 解析失败: %w", err)
	}
	return j.Email, nil
}

// LoadCodeAssist 问账号的 Cloud 项目与 tier。
//
// metadata.ideType=ANTIGRAVITY 是必须的；project 为空表示「问默认项目」。
// prod 域优先，不通再回落 daily（对齐 Java 的 loadBase → chatBase 顺序）。
func LoadCodeAssist(ctx context.Context, access, project string) (map[string]any, error) {
	body := map[string]any{
		"metadata": map[string]any{"ideType": IdeType},
	}
	if project != "" {
		body["cloudaicompanionProject"] = project
	}
	out, err := postJSON(ctx, LoadBase+PathLoadCodeAssist, access, body, GoogleAPIUserAgent)
	if err == nil {
		return out, nil
	}
	return postJSON(ctx, ReqBase+PathLoadCodeAssist, access, body, GoogleAPIUserAgent)
}

// ProjectOf 从 loadCodeAssist 响应里取项目 id（字符串或对象两种形态）。
func ProjectOf(load map[string]any) string {
	if load == nil {
		return ""
	}
	switch v := load["cloudaicompanionProject"].(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["id"].(string); ok {
			return s
		}
	}
	return ""
}

// TierOf 取 currentTier.id。
func TierOf(load map[string]any) string { return nestedString(load, "currentTier", "id") }

// PaidTierOf 取 paidTier.id。
func PaidTierOf(load map[string]any) string { return nestedString(load, "paidTier", "id") }

// PlanOf 取套餐展示名（paidTier.name 优先，回退 currentTier.name / id）。
func PlanOf(load map[string]any) string {
	if n := nestedString(load, "paidTier", "name"); n != "" {
		return n
	}
	if n := nestedString(load, "currentTier", "name"); n != "" {
		return n
	}
	return nestedString(load, "currentTier", "id")
}

func nestedString(root map[string]any, outer, inner string) string {
	if root == nil {
		return ""
	}
	m, ok := root[outer].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[inner].(string)
	return s
}

// Onboard 给新账号领一个 Cloud 项目（LRO，最多轮 5 次、每次隔 2 秒）。
func Onboard(ctx context.Context, access string) (string, error) {
	body := map[string]any{
		"tier_id": OnboardTier,
		"metadata": map[string]any{
			"ide_type":    IdeType,
			"ide_version": AppVersion,
			"ide_name":    "antigravity",
		},
	}
	for i := 0; i < 5; i++ {
		lro, err := postJSON(ctx, ReqBase+PathOnboardUser, access, body, GoogleAPIUserAgent)
		if err != nil {
			return "", err
		}
		if done, _ := lro["done"].(bool); done {
			if resp, ok := lro["response"].(map[string]any); ok {
				switch v := resp["cloudaicompanionProject"].(type) {
				case string:
					return v, nil
				case map[string]any:
					if s, ok := v["id"].(string); ok {
						return s, nil
					}
				}
			}
			return "", nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", nil
}

// EnsureProject 保证账号有项目，没有就 loadCodeAssist + onboard 补一次。
func EnsureProject(ctx context.Context, acc *Account, access string) (string, error) {
	if acc == nil {
		return "", errors.New("antigravity: account 不能为空")
	}
	if acc.Project != "" {
		return acc.Project, nil
	}
	load, err := LoadCodeAssist(ctx, access, "")
	if err != nil {
		return "", err
	}
	project := ProjectOf(load)
	if project == "" {
		project, err = Onboard(ctx, access)
		if err != nil {
			return "", err
		}
	}
	if project == "" {
		return "", errors.New("antigravity: Google 没有给这个账号分配 Cloud 项目（onboard 未完成）")
	}
	acc.Project = project
	acc.Tier = TierOf(load)
	acc.PaidTier = PaidTierOf(load)
	if p := PlanOf(load); p != "" {
		acc.Plan = p
	}
	return project, nil
}

// FetchModels 拉模型清单（含配额），过滤掉非聊天模型与 disabled。
//
// 先按 paidTier / tier 带 entitlement 问，最后再退回不带 entitlement 的通用档 ——
// 有些 tier 账号其实没有，Google 会回 400，所以要按顺序试。
func FetchModels(ctx context.Context, acc *Account, access, project string) ([]Model, error) {
	tiers := make([]string, 0, 3)
	add := func(t string) {
		if t == "" || t == "free-tier" || t == "legacy-tier" {
			return
		}
		for _, e := range tiers {
			if e == t {
				return
			}
		}
		tiers = append(tiers, t)
	}
	if acc != nil {
		add(acc.PaidTier)
		add(acc.Tier)
	}
	tiers = append(tiers, "")

	var lastErr error
	var raw map[string]any
	for _, t := range tiers {
		body := map[string]any{"project": project}
		if t != "" {
			body["entitlement"] = map[string]any{"userTier": t}
		}
		res, err := postJSON(ctx, ReqBase+PathFetchModels, access, body, UserAgent)
		if err != nil {
			lastErr = err
			continue
		}
		raw = res
		if acc != nil && t != "" {
			acc.Tier = t
		}
		break
	}
	if raw == nil {
		if lastErr == nil {
			lastErr = errors.New("antigravity: 模型列表拉取失败")
		}
		return nil, lastErr
	}

	models, ok := raw["models"].(map[string]any)
	if !ok {
		return nil, nil
	}
	out := make([]Model, 0, len(models))
	for id, v := range models {
		if isHidden(id) {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if disabled, _ := m["disabled"].(bool); disabled {
			continue
		}
		item := Model{ID: id, Name: id}
		if n, ok := m["displayName"].(string); ok && n != "" {
			item.Name = n
		}
		if n, ok := m["maxTokens"].(float64); ok && n > 0 {
			item.Context = int(n)
		}
		if n, ok := m["maxOutputTokens"].(float64); ok && n > 0 {
			item.Output = int(n)
		}
		if q, ok := m["quotaInfo"].(map[string]any); ok {
			if f, ok := q["remainingFraction"].(float64); ok {
				item.Remaining = f
			}
			if s, ok := q["resetTime"].(string); ok {
				item.Reset = s
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func isHidden(id string) bool {
	for _, h := range HiddenModels {
		if h == id {
			return true
		}
	}
	return false
}

// ===================== 聊天信封 =====================

// ChatMessage 是 OpenAI 风格的一条消息（本包只处理纯文本内容）。
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatPayload 是构造信封需要的那部分客户端请求字段。
type ChatPayload struct {
	Messages    []ChatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

// Envelope 是 Code Assist 的请求信封。
//
// 上游结构：{"model","project","userAgent","requestType","requestId","request"}，
// request 里是 contents / systemInstruction / generationConfig / sessionId。
type Envelope struct {
	Model       string         `json:"model"`
	Project     string         `json:"project"`
	UserAgent   string         `json:"userAgent"`
	RequestType string         `json:"requestType"`
	RequestID   string         `json:"requestId"`
	Request     map[string]any `json:"request"`
}

// BuildEnvelope 把一段 OpenAI 风格的消息翻成 Code Assist 信封（**只支持纯文本**）。
//
// 工具调用、图片、thoughtSignature、连续同角色合并等换算本包没做
// （那些在 AntigravityClient.buildEnvelope 里，体量很大）；接线上需要时再补。
func BuildEnvelope(p *ChatPayload, model, project string) (*Envelope, error) {
	if p == nil || len(p.Messages) == 0 {
		return nil, errors.New("antigravity: 需要消息")
	}
	contents := make([]map[string]any, 0, len(p.Messages))
	var sysParts []string
	for _, m := range p.Messages {
		switch m.Role {
		case "system", "developer":
			if m.Content != "" {
				sysParts = append(sysParts, m.Content)
			}
			continue
		case "user":
		case "assistant", "model":
		default:
			return nil, fmt.Errorf("antigravity: 不支持的消息角色 %q", m.Role)
		}
		role := "user"
		if m.Role == "assistant" || m.Role == "model" {
			role = "model"
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": []map[string]any{{"text": m.Content}},
		})
	}
	if len(contents) == 0 {
		return nil, errors.New("antigravity: 没有可发送的对话内容")
	}
	// Gemini 不接受以 model 结尾的历史。
	if contents[len(contents)-1]["role"] == "model" {
		contents = append(contents, map[string]any{
			"role":  "user",
			"parts": []map[string]any{{"text": "Continue."}},
		})
	}

	req := map[string]any{
		"contents":  contents,
		"sessionId": SessionID(contents),
	}
	if len(sysParts) > 0 {
		sys := strings.Join(sysParts, "\n\n")
		req["systemInstruction"] = map[string]any{
			"role":  "user",
			"parts": []map[string]any{{"text": CloakSystem(sys)}},
		}
	}
	gen := map[string]any{}
	if p.Temperature != nil {
		gen["temperature"] = *p.Temperature
	}
	if p.MaxTokens > 0 {
		gen["maxOutputTokens"] = p.MaxTokens
	}
	if len(gen) > 0 {
		req["generationConfig"] = gen
	}

	return &Envelope{
		Model:       model,
		Project:     project,
		UserAgent:   EnvelopeUserAgent,
		RequestType: RequestType,
		RequestID:   "agent-" + randomHex(12),
		Request:     req,
	}, nil
}

// SessionID 同一段对话必须同一个 sessionId：首条用户消息 sha256 的低 63 位，带前导 "-"。
//
// 首条用户消息为空时退化成随机 16 字节 hex（对齐 Java 的兜底）。
func SessionID(contents []map[string]any) string {
	first := ""
	for _, c := range contents {
		if c["role"] != "user" {
			continue
		}
		if parts, ok := c["parts"].([]map[string]any); ok {
			for _, p := range parts {
				if t, ok := p["text"].(string); ok && t != "" {
					first = t
					break
				}
			}
		}
		break
	}
	if first == "" {
		return "-" + randomHex(16)
	}
	sum := sha256.Sum256([]byte(first))
	var n uint64
	for i := 0; i < 8; i++ {
		n = (n << 8) | uint64(sum[i])
	}
	n &= 0x7fffffffffffffff
	return fmt.Sprintf("-%d", n)
}

// TurnsAway 判断这段 system prompt 是否会触发 Antigravity 的假 429。
func TurnsAway(system string) bool {
	if system == "" {
		return false
	}
	low := strings.ToLower(system)
	for _, line := range TurnedAway {
		if strings.Contains(low, line) {
			return true
		}
	}
	return false
}

// CloakSystem 在命中 TurnedAway 的片段第一个字符后插一个 U+200B。
//
// Google 的检查过不去，模型照读（upstream antigravityCloak 同款做法）。
func CloakSystem(system string) string {
	if system == "" || !TurnsAway(system) {
		return system
	}
	low := strings.ToLower(system)
	at := make(map[int]bool)
	for _, line := range TurnedAway {
		i := 0
		for {
			j := strings.Index(low[i:], line)
			if j < 0 {
				break
			}
			at[i+j+1] = true
			i += j + len(line)
		}
	}
	if len(at) == 0 {
		return system
	}
	var b strings.Builder
	for i := 0; i <= len(system); i++ {
		if at[i] {
			b.WriteString(ZeroWidth)
		}
		if i < len(system) {
			b.WriteByte(system[i])
		}
	}
	return b.String()
}

// StreamRequest 构造聊天请求（尚未发送）。
//
// 默认打到 ReqBase + StreamPath；要换域（LoadBase）时改 URL 后自行重建请求。
func StreamRequest(ctx context.Context, acc *Account, env *Envelope) (*http.Request, error) {
	if env == nil {
		return nil, errors.New("antigravity: 信封为空")
	}
	buf, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ReqBase+StreamPath, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	for k, v := range Headers(acc) {
		req.Header.Set(k, v)
	}
	return req, nil
}

// ===================== HTTP 底座 =====================

// UpstreamError 带上 HTTP 状态，方便上层区分「重试」与「换账号」。
type UpstreamError struct {
	Op      string
	Status  int
	Message string
}

// Error 实现 error。
func (e *UpstreamError) Error() string {
	return fmt.Sprintf("antigravity: %s 失败 (http=%d): %s", e.Op, e.Status, e.Message)
}

// StatusOf 取错误里的 HTTP 状态码，不是 UpstreamError 返回 0。
func StatusOf(err error) int {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Status
	}
	return 0
}

func shortErr(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// tokenResponse 是 Google OAuth token 端点的响应（成功与失败共用）。
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// googleTokenError 把 Google OAuth 的错误翻成人话（Java 侧 googleTokenError 同款口径）。
func googleTokenError(tok tokenResponse) error {
	text := strings.ToLower(tok.Error + " " + tok.ErrorDescription)
	switch {
	case strings.Contains(text, "invalid_grant"):
		if strings.Contains(text, "malformed") {
			return errors.New("antigravity: 授权码格式不对（多半是复制时被截断），请重新点登录拿新链接")
		}
		return errors.New("antigravity: 授权码已失效或被用过（一个码只能用一次），请重新登录")
	case strings.Contains(text, "invalid_client"):
		return errors.New("antigravity: OAuth 客户端不被 Google 接受（client_id / secret 可能变了）")
	case strings.Contains(text, "redirect_uri_mismatch"):
		return errors.New("antigravity: 回调地址不匹配：Google 只接受 http://localhost:1456/oauth-callback")
	case strings.Contains(text, "access_denied"):
		return errors.New("antigravity: 你在 Google 授权页点了拒绝")
	case strings.Contains(text, "unsupported_country"), strings.Contains(text, "region"):
		return errors.New("antigravity: 这个 Google 账号所在地区不支持 Antigravity")
	}
	if tok.Error != "" {
		return fmt.Errorf("antigravity: Google 返回 %s: %s", tok.Error, shortErr(tok.ErrorDescription))
	}
	return errors.New("antigravity: Google 没有返回 access_token")
}

// postForm 发 application/x-www-form-urlencoded（token 端点用）。
//
// token 端点的错误体也是 JSON（invalid_grant 之类），失败时照样解析给上层判断。
func postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	body, status, err := do(req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		if status >= 200 && status < 300 {
			return fmt.Errorf("antigravity: token 响应解析失败: %w", err)
		}
		return &UpstreamError{Op: "token", Status: status, Message: shortErr(string(body))}
	}
	return nil
}

// postJSON 发 JSON 请求并解析响应。
func postJSON(ctx context.Context, endpoint, bearer string, payload any, ua string) (map[string]any, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	body, err := doJSON(ctx, http.MethodPost, endpoint, bearer, buf, ua)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(body)) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("antigravity: 响应不是 JSON（%s）", shortErr(string(body)))
	}
	return out, nil
}

func doJSON(ctx context.Context, method, endpoint, bearer string, payload []byte, ua string) ([]byte, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	body, status, err := do(req)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 400 {
		return nil, &UpstreamError{Op: "request", Status: status, Message: shortErr(string(body))}
	}
	return body, nil
}

// do 发请求，返回响应体与状态码（非 2xx 不在这里报错，由调用方决定语义）。
func do(req *http.Request) ([]byte, int, error) {
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// randomHex 生成 n 字节随机 hex（Java 的 randomToken(n) 等价物）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(i * 7)
		}
	}
	return hex.EncodeToString(b)
}
