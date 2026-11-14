// Package copilot 适配 GitHub Copilot 供应商（api.githubcopilot.com）。
//
// 认证链路是 GitHub 官方 OAuth 设备流（device flow），全程免本地回调端口、免 API Key：
//
//  1. POST https://github.com/login/device/code
//     {"client_id":"Iv1.b507a08c87ecfe98","scope":"read:user"}
//     → device_code / user_code / verification_uri / expires_in / interval
//  2. 用户在浏览器打开 verification_uri 并输入 user_code
//  3. 轮询 POST https://github.com/login/oauth/access_token
//     {"client_id","device_code","grant_type":"urn:ietf:params:oauth:grant-type:device_code"}
//     authorization_pending 期间继续等；成功后拿到「长期」GitHub user token
//  4. GET https://api.github.com/copilot_internal/v2/token（Authorization: Bearer <GitHub token>）
//     → Copilot 短期 token（约 30 分钟）+ expires_at（epoch 秒）
//  5. 转发聊天请求到 https://api.githubcopilot.com/chat/completions（OpenAI 兼容）
//
// 账号字段约定（与 All-in 的 config.providers[].accounts[] 一致）：
//
//	Refresh        = GitHub user token（长期，用来换 Copilot token）
//	Access / Token = Copilot 短期 token（真正发给上游的那个）
//	Expires        = Copilot token 到期毫秒时间戳
//
// 关键点：Copilot 的推理端点只认 Bearer + 一整套客户端指纹头，少一个就是 401
// （哪怕 token 完全有效）。指纹头见 Headers / HeadersFor。
package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 协议常量。全部对应 Java 侧 CopilotAuth 里硬编码的那一套。
const (
	// ProviderID 是供应商 id（配置里的 providers[].id）。
	ProviderID = "github-copilot"

	// ClientID 是 GitHub Copilot 官方 VS Code 扩展内置的 OAuth App id（公开、固定）。
	ClientID = "Iv1.b507a08c87ecfe98"

	// DeviceCodeURL 申请设备码。
	DeviceCodeURL = "https://github.com/login/device/code"
	// AccessTokenURL 轮询换取 GitHub user token。
	AccessTokenURL = "https://github.com/login/oauth/access_token"
	// CopilotTokenURL 用 GitHub token 换 Copilot 短期 token。
	CopilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	// AllowanceURL 只读额度查询（需要 GitHub token，不要拿 Copilot token 去问）。
	AllowanceURL = "https://api.github.com/copilot_internal/user"
	// UserURL 取 GitHub 登录名。
	UserURL = "https://api.github.com/user"

	// BaseURL 是 Copilot 的 OpenAI 兼容端点根。
	BaseURL = "https://api.githubcopilot.com"

	// GrantDevice 是设备流固定的 grant_type。
	GrantDevice = "urn:ietf:params:oauth:grant-type:device_code"

	// Scope 只要读用户信息，够用来判断套餐。
	Scope = "read:user"
)

// 客户端指纹。这些值在 Java 侧是写死的常量，上游按它判「是不是真 Copilot 客户端」。
const (
	// UserAgent 伪装成 Copilot Chat 扩展。
	UserAgent = "GitHubCopilotChat/0.68.0"
	// EditorVersion 伪装成 VS Code 版本。
	EditorVersion = "vscode/1.140.0"
	// PluginVersion 伪装成 Copilot Chat 扩展版本。
	PluginVersion = "copilot-chat/0.68.0"
	// IntegrationID 是必填的集成标识，缺了直接 401。
	IntegrationID = "vscode-chat"
	// OpenAIIntent 是必填的意图头。
	OpenAIIntent = "conversation-edits"
)

// X-Initiator 的两个取值。
const (
	// InitiatorUser 普通提问。
	InitiatorUser = "user"
	// InitiatorAgent 工具链 / assistant 续写，算 agent 回合。
	InitiatorAgent = "agent"
)

// tokenRefreshLeeway 是提前续期的安全余量：剩余不足 5 分钟就重换。
const tokenRefreshLeeway = 5 * time.Minute

// maxBody 限制单次响应体读取量，防止上游异常时把内存吃满。
const maxBody = 1 << 20

// DefaultModels 是网络不通 / 模型目录拉取失败时给面板用的兜底清单。
// 真实清单一律以 /models 返回为准。
var DefaultModels = []string{
	"gpt-5.1",
	"gpt-5.1-codex",
	"gpt-5.1-codex-mini",
	"gpt-5",
	"gpt-5-mini",
	"claude-sonnet-4.5",
	"claude-opus-4.1",
	"gemini-3-pro-preview",
	"o3",
	"o4-mini",
}

// 轮询状态量。用哨兵错误让调用方自己决定节奏（不需要 sleep 在里面）。
var (
	// ErrAuthorizationPending 表示用户还没在浏览器里完成授权，继续等。
	ErrAuthorizationPending = errors.New("copilot: 用户尚未完成设备授权")
	// ErrSlowDown 表示轮询过快，GitHub 要求放慢（本包会自动把间隔 +5s）。
	ErrSlowDown = errors.New("copilot: 轮询过快，需要放慢")
)

// HTTPClient 是这个包用的客户端。只做鉴权与模型目录，不转发流式响应，
// 所以可以设总超时；真正转发请用 provider 包里的 HTTPClient。
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// DeviceFlow 是一次进行中的设备码登录。
type DeviceFlow struct {
	// DeviceCode 是 GitHub 发给我们的设备码，轮询时才用，不要给用户看。
	DeviceCode string `json:"deviceCode"`
	// UserCode 是给用户看、让他在网页里输入的短码（形如 WDJB-MJHT）。
	UserCode string `json:"userCode"`
	// VerificationURI 是用户要打开的地址。
	VerificationURI string `json:"verificationUri"`
	// VerificationURIComplete 是带上 user_code 的地址。
	//
	// 注意：GitHub 并不返回 verification_uri_complete，而且实测这个 query 参数
	// 也不会帮用户预填（Java 侧靠注入 JS 才能自动填），只当"省一次复制"用。
	VerificationURIComplete string `json:"verificationUriComplete"`
	// ExpiresIn 是设备码有效期（秒）。
	ExpiresIn int64 `json:"expiresIn"`
	// Interval 是建议的轮询间隔（下限 3 秒）。
	Interval time.Duration `json:"-"`
	// IntervalMs 是 Interval 的毫秒形式，便于直接塞进 JSON 给前端。
	IntervalMs int64 `json:"intervalMs"`
	// CreatedAt 是发起时间，用来判过期与做清理。
	CreatedAt time.Time `json:"createdAt"`
}

// ExpiresAt 返回设备码过期时刻。
func (f *DeviceFlow) ExpiresAt() time.Time {
	if f == nil || f.ExpiresIn <= 0 {
		return time.Time{}
	}
	return f.CreatedAt.Add(time.Duration(f.ExpiresIn) * time.Second)
}

// Expired 判断设备码是否已过期。
func (f *DeviceFlow) Expired() bool {
	if f == nil {
		return true
	}
	exp := f.ExpiresAt()
	return !exp.IsZero() && time.Now().After(exp)
}

// Account 是一个 Copilot 账号的凭据集合。
type Account struct {
	ID string `json:"id,omitempty"`
	// Name 是面板上显示的名字（一般用 GitHub 登录名）。
	Name string `json:"name,omitempty"`
	// Type 固定 oauth。
	Type string `json:"type,omitempty"`
	// GitHubLogin 是 GitHub 用户名。
	GitHubLogin string `json:"githubLogin,omitempty"`
	// Refresh 存 GitHub user token（长期），换 Copilot token 时才用。
	Refresh string `json:"refresh,omitempty"`
	// Access 存 Copilot 短期 token（约 30 分钟）。
	Access string `json:"access,omitempty"`
	// Token 与 Access 同值，兼容只读 token 字段的老客户端。
	Token string `json:"token,omitempty"`
	// Expires 是 Access 的到期毫秒时间戳。
	Expires int64 `json:"expires,omitempty"`
	// AddedAt 是入池时间（毫秒）。
	AddedAt int64 `json:"addedAt,omitempty"`
}

// CopilotToken 返回当前手里的短期 token（Access 优先，回退 Token）。
func (a *Account) CopilotToken() string {
	if a == nil {
		return ""
	}
	if a.Access != "" {
		return a.Access
	}
	return a.Token
}

// Expired 判断短期 token 是否已过期（Expires 为 0 视为未知，按未过期处理）。
func (a *Account) Expired() bool {
	if a == nil || a.Expires <= 0 {
		return false
	}
	return time.Now().UnixMilli() >= a.Expires
}

// setToken 把换来的短期 token 写回账号。
func (a *Account) setToken(tok string, expiresMS int64) {
	a.Access = tok
	a.Token = tok
	a.Expires = expiresMS
}

// StartDeviceFlow 申请一组设备码，返回给用户看的信息。
//
// 调用方拿到结果后应当：把 VerificationURIComplete / UserCode 展示给用户，
// 然后按 flow.Interval 的节奏反复调用 PollDeviceFlow。
func StartDeviceFlow(ctx context.Context) (*DeviceFlow, error) {
	payload := map[string]any{
		"client_id": ClientID,
		"scope":     Scope,
	}
	status, body, err := postJSON(ctx, DeviceCodeURL, payload)
	if err != nil {
		return nil, fmt.Errorf("申请 GitHub 设备码失败: %w", err)
	}
	if status < 200 || status >= 400 {
		return nil, fmt.Errorf("申请 GitHub 设备码失败 (HTTP %d): %s", status, snippet(body))
	}

	var resp struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析设备码响应失败: %w（原始内容 %s）", err, snippet(body))
	}
	if resp.DeviceCode == "" || resp.UserCode == "" || resp.VerificationURI == "" {
		return nil, fmt.Errorf("GitHub 设备码返回不完整: %s", snippet(body))
	}

	// GitHub 不返回 verification_uri_complete，这里自己拼一个方便用户一键打开。
	complete := resp.VerificationURIComplete
	if complete == "" {
		complete = resp.VerificationURI + "?user_code=" + url.QueryEscape(resp.UserCode)
	}

	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900 // 缺省 15 分钟
	}
	interval := resp.Interval
	if interval <= 0 {
		interval = 5
	}
	// 实测间隔低于 3 秒会立刻被 slow_down 打回，这里直接抬到 3 秒。
	intervalMs := interval * 1000
	if intervalMs < 3000 {
		intervalMs = 3000
	}

	return &DeviceFlow{
		DeviceCode:              resp.DeviceCode,
		UserCode:                resp.UserCode,
		VerificationURI:         resp.VerificationURI,
		VerificationURIComplete: complete,
		ExpiresIn:               expiresIn,
		Interval:                time.Duration(intervalMs) * time.Millisecond,
		IntervalMs:              intervalMs,
		CreatedAt:               time.Now(),
	}, nil
}

// PollDeviceFlow 轮询一次设备流。
//
// 返回：
//   - (*Account, nil)              授权完成，账号凭据已就绪（GitHub token + Copilot token）
//   - (nil, ErrAuthorizationPending) 用户还没操作完，按 flow.Interval 继续等
//   - (nil, ErrSlowDown)           被要求放慢（间隔已自动 +5 秒）
//   - (nil, 其它 error)            流程作废（设备码过期 / 用户拒绝 / 该账号没有 Copilot 订阅）
func PollDeviceFlow(ctx context.Context, flow *DeviceFlow) (*Account, error) {
	if flow == nil || flow.DeviceCode == "" {
		return nil, errors.New("copilot: 登录流程无效，请重新发起")
	}
	if flow.Expired() {
		return nil, errors.New("copilot: 登录超时，请重新发起")
	}

	status, body, err := postJSON(ctx, AccessTokenURL, map[string]any{
		"client_id":   ClientID,
		"device_code": flow.DeviceCode,
		"grant_type":  GrantDevice,
	})
	if err != nil {
		// 单次网络抖动不算失败，让调用方继续轮询。
		return nil, fmt.Errorf("%w: %v", ErrAuthorizationPending, err)
	}

	var resp struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
		Interval    int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		// GitHub 偶尔回 HTML / 空串，按"还没好"处理。
		return nil, fmt.Errorf("%w: 非 JSON 响应 (HTTP %d)", ErrAuthorizationPending, status)
	}

	if resp.AccessToken == "" {
		switch resp.Error {
		case "", "authorization_pending":
			return nil, ErrAuthorizationPending
		case "slow_down":
			// GitHub 规定：让放慢就 +5 秒，且后续请求按新间隔走。
			flow.Interval += 5 * time.Second
			flow.IntervalMs = flow.Interval.Milliseconds()
			if resp.Interval > 0 {
				flow.Interval = time.Duration(resp.Interval) * time.Second
				flow.IntervalMs = flow.Interval.Milliseconds()
			}
			return nil, ErrSlowDown
		case "expired_token":
			return nil, errors.New("copilot: 设备码已过期，请重新发起登录")
		case "access_denied":
			return nil, errors.New("copilot: 用户在 GitHub 上拒绝了授权")
		case "incorrect_device_code":
			return nil, errors.New("copilot: 设备码不正确，请重新发起登录")
		case "device_flow_disabled":
			return nil, errors.New("copilot: 该 OAuth App 未启用设备流")
		default:
			msg := resp.Description
			if msg == "" {
				msg = snippet(body)
			}
			return nil, fmt.Errorf("copilot: GitHub 拒绝了设备授权: %s", msg)
		}
	}

	// GitHub 授权成功 → 换 Copilot 短期 token。
	acc := &Account{
		Type:        "oauth",
		Refresh:     resp.AccessToken,
		AddedAt:     time.Now().UnixMilli(),
		GitHubLogin: fetchLogin(ctx, resp.AccessToken), // 失败留空，不影响可用性
	}
	tok, expiresMS, err := ExchangeToken(ctx, resp.AccessToken)
	if err != nil {
		if errors.Is(err, ErrNoCopilotSeat) {
			return nil, err
		}
		return nil, fmt.Errorf("换 Copilot token 失败: %w", err)
	}
	acc.setToken(tok, expiresMS)
	if acc.GitHubLogin != "" {
		acc.Name = acc.GitHubLogin
	} else {
		acc.Name = "GitHub Copilot"
	}
	return acc, nil
}

// ErrNoCopilotSeat 表示 GitHub 授权成功，但该账号没有 Copilot 订阅 / 组织未启用。
var ErrNoCopilotSeat = errors.New("copilot: GitHub 授权成功，但换不到 Copilot token（该账号没有 Copilot 订阅，或未在组织里启用）")

// ExchangeToken 用 GitHub user token 换 Copilot 短期 token。
//
// 返回 token 与到期毫秒时间戳；上游没给 expires_at 时按 25 分钟兜底。
func ExchangeToken(ctx context.Context, githubToken string) (string, int64, error) {
	githubToken = strings.TrimSpace(githubToken)
	if githubToken == "" {
		return "", 0, errors.New("copilot: GitHub token 为空")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CopilotTokenURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Editor-Version", EditorVersion)
	req.Header.Set("Editor-Plugin-Version", PluginVersion)
	req.Header.Set("Authorization", "Bearer "+githubToken)

	status, body, err := do(req)
	if err != nil {
		return "", 0, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "", 0, ErrNoCopilotSeat
	}
	if status < 200 || status >= 300 {
		return "", 0, fmt.Errorf("Copilot token 交换失败 (HTTP %d): %s", status, snippet(body))
	}

	var resp struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"` // epoch 秒
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", 0, fmt.Errorf("解析 Copilot token 响应失败: %w", err)
	}
	if resp.Token == "" {
		return "", 0, ErrNoCopilotSeat
	}

	expiresMS := resp.ExpiresAt * 1000
	if resp.ExpiresAt <= 0 {
		expiresMS = time.Now().Add(25 * time.Minute).UnixMilli()
	}
	return resp.Token, expiresMS, nil
}

// RefreshToken 用账号里的 GitHub token 重新换一个 Copilot token，并就地更新 acc。
//
// 与 Java 侧 refreshCandidate 的差别：这里要求 GitHub token 必须存在，
// 缺失时报错而不是静默回退旧 token —— 上层看到错误才好提示"重新登录"。
func RefreshToken(ctx context.Context, acc *Account) error {
	if acc == nil {
		return errors.New("copilot: 账号为空")
	}
	if strings.TrimSpace(acc.Refresh) == "" {
		return errors.New("copilot: 账号缺少 GitHub token，需要重新登录")
	}
	tok, expiresMS, err := ExchangeToken(ctx, acc.Refresh)
	if err != nil {
		return err
	}
	acc.setToken(tok, expiresMS)
	return nil
}

// EnsureToken 保证账号手里有一个可用的 Copilot token。
//
// 剩余有效期不足 5 分钟就重换。重换失败时回退旧 token（可能已过期，
// 让上游 401 报出去，比在本地吞掉更接近真实）。
func EnsureToken(ctx context.Context, acc *Account) (string, error) {
	if acc == nil {
		return "", errors.New("copilot: 账号为空")
	}
	tok := acc.CopilotToken()
	if tok != "" {
		// Expires 为 0 表示到期时间未知，按可用处理。
		if acc.Expires <= 0 || time.Now().Add(tokenRefreshLeeway).UnixMilli() < acc.Expires {
			return tok, nil
		}
	}
	if err := RefreshToken(ctx, acc); err != nil {
		if tok != "" {
			return tok, nil
		}
		return "", err
	}
	return acc.CopilotToken(), nil
}

// Headers 返回转发聊天请求所需的完整请求头（含 Authorization）。
//
// X-Initiator 默认 user；要区分 agent 回合 / 带图请求请用 HeadersFor
// 或 ChatHeaders。
func Headers(acc *Account) map[string]string {
	return HeadersFor(acc, InitiatorUser, false)
}

// HeadersFor 按需组装请求头。
//
// initiator 取 user 或 agent；vision 为 true 时补 Copilot-Vision-Request。
// 这几组头缺任何一个上游都直接 401，跟 token 对不对无关。
func HeadersFor(acc *Account, initiator string, vision bool) map[string]string {
	if initiator != InitiatorAgent {
		initiator = InitiatorUser
	}
	h := map[string]string{
		"User-Agent":             UserAgent,
		"Editor-Version":         EditorVersion,
		"Editor-Plugin-Version":  PluginVersion,
		"Copilot-Integration-Id": IntegrationID,
		"Openai-Intent":          OpenAIIntent,
		"X-Initiator":            initiator,
	}
	if vision {
		h["Copilot-Vision-Request"] = "true"
	}
	if tok := acc.CopilotToken(); tok != "" {
		h["Authorization"] = "Bearer " + tok
	}
	return h
}

// ChatHeaders 从原始请求体里推断 X-Initiator 与是否带图，再套 HeadersFor。
//
// 即 Java 侧 isAgentCall + isVisionRequest + clientHeaders 的组合用法：
// 最后一个消息是 tool / assistant → agent；content 里出现图片块 → vision。
func ChatHeaders(acc *Account, body []byte) map[string]string {
	initiator := InitiatorUser
	if IsAgentCall(body) {
		initiator = InitiatorAgent
	}
	return HeadersFor(acc, initiator, IsVisionRequest(body))
}

// Models 拉 Copilot 真实模型目录（GET /models，需要 Copilot token）。
//
// 只取 id 去重，返回顺序与上游一致。返回空切片且 err 为 nil 表示上游没给模型。
func Models(ctx context.Context, acc *Account) ([]string, error) {
	tok, err := EnsureToken(ctx, acc)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, errors.New("copilot: 没有可用的 Copilot token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range HeadersFor(nil, InitiatorUser, false) {
		// 覆盖 User-Agent / 指纹头；Authorization 由上面单独设，这里跳过 nil 账号带来的空值。
		if k == "Authorization" {
			continue
		}
		req.Header.Set(k, v)
	}

	status, body, err := do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取 Copilot 模型目录失败: %w", err)
	}
	if status < 200 || status >= 400 {
		return nil, fmt.Errorf("Copilot 模型目录 HTTP %d: %s", status, snippet(body))
	}

	// 上游可能是 {"data":[{id:...}]}（OpenAI 风格），也可能是 {"models":[...]}。
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析模型目录失败: %w", err)
	}
	out := make([]string, 0, len(resp.Data)+len(resp.Models))
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, m := range resp.Data {
		add(m.ID)
	}
	for _, m := range resp.Models {
		add(m.ID)
	}
	return out, nil
}

// IsAgentCall 判断这次请求算不算 agent 回合（工具链回包、assistant 续写）。
//
// 规则与 Java 侧 isAgentCall 一致：
//   - messages 风格：最后一条 role 是 tool 或 assistant
//   - responses 风格：最后一条 input 的 role 是 assistant，或 type 不是 message
func IsAgentCall(body []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	if msgs, ok := decodeArray(m["messages"]); ok && len(msgs) > 0 {
		if role, ok := stringField(msgs[len(msgs)-1], "role"); ok {
			return role == "tool" || role == "assistant"
		}
	}
	if in, ok := decodeArray(m["input"]); ok && len(in) > 0 {
		last := in[len(in)-1]
		if role, ok := stringField(last, "role"); ok && role == "assistant" {
			return true
		}
		if typ, ok := stringField(last, "type"); ok && typ != "" && typ != "message" {
			return true
		}
	}
	return false
}

// IsVisionRequest 判断请求里是否带图（带图必须补 Copilot-Vision-Request）。
func IsVisionRequest(body []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	for _, field := range []string{"messages", "input"} {
		items, ok := decodeArray(m[field])
		if !ok {
			continue
		}
		for _, item := range items {
			content, ok := decodeArray(item["content"])
			if !ok {
				continue
			}
			for _, part := range content {
				typ, ok := stringField(part, "type")
				if !ok {
					continue
				}
				if typ == "image_url" || typ == "input_image" {
					return true
				}
			}
		}
	}
	return false
}

// decodeArray 把一段原始 JSON 解成「每个元素仍是原始 JSON 对象」的切片。
func decodeArray(raw json.RawMessage) ([]map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false
	}
	return arr, true
}

// stringField 取对象里的字符串字段。
func stringField(obj map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// fetchLogin 读 GitHub 登录名，失败返回空串（不影响凭据可用性）。
func fetchLogin(ctx context.Context, githubToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UserURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Authorization", "Bearer "+githubToken)

	status, body, err := do(req)
	if err != nil || status < 200 || status >= 400 {
		return ""
	}
	var resp struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	return strings.TrimSpace(resp.Login)
}

// postJSON 发一个 JSON POST，返回状态码与响应体（错误响应体也会读回来）。
func postJSON(ctx context.Context, endpoint string, payload any) (int, []byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	return do(req)
}

// do 执行请求并读完响应体。
func do(req *http.Request) (int, []byte, error) {
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// snippet 把响应体裁成一行短文本，避免错误信息里塞整个 JSON。
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		return "(空响应)"
	}
	return s
}
