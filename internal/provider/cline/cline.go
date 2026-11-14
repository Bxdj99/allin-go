// Package cline 封装 Cline（cline.bot）的认证链路与协议常量。
//
// 移植自 All-in 的 ClineAuth.java（认证部分）、ClineFailurePolicy.java、
// ClineRoutingPolicy.java 与 GatewayServer 里 cline 的那几条分支。
//
// 认证链路（两条路，互斥）：
//
//  1. WorkOS 设备流（OAuth，本包实现的主线）
//     a. POST https://api.workos.com/user_management/authorize/device
//     form: client_id=client_01K3A541FN8TA3EPPHTD2325AR
//     → device_code / user_code / verification_uri / expires_in / interval
//     用户打开 verification_uri 输 user_code（或直接开 verification_uri_complete）。
//     b. POST https://api.workos.com/user_management/authenticate
//     form: grant_type=urn:ietf:params:oauth:grant-type:device_code
//     &device_code=…&client_id=…
//     → access_token / refresh_token（WorkOS 自己的令牌）
//     未完成时返回 error=authorization_pending（偶尔 slow_down）。
//     c. POST https://api.cline.bot/api/v1/auth/register
//     json: {"accessToken":<WorkOS access>,"refreshToken":<WorkOS refresh>}
//     → Cline 账号真实的令牌对（data.accessToken / data.refreshToken /
//     data.expiresAt / data.userInfo）
//
//     PKCE 不适用于 Cline：WorkOS 这条是「公开客户端 + 设备码」，服务端只认
//     client_id，没有 code_challenge / code_verifier 参数。Java 侧同样没有 PKCE
//     代码，别照 MiniMax 那套加。
//
//  2. Cline API Key：app.cline.bot 生成的个人 Key，直接当普通 Bearer 用
//     （账号 Kind="api"，不加 workos: 前缀）。
//
// 续期：POST https://api.cline.bot/api/v1/auth/refresh
//
//	json: {"refreshToken": …, "grantType": "refresh_token"}
//	→ data.accessToken / data.refreshToken / data.expiresAt
//
// refresh token 是一次性的：请求体一旦可能交付给上游，就不能拿旧值重放
// （ClineAuth 整套 RefreshDelivery/Refusal 状态机就是为这条写的）。本包按
// 「成功才改账号、失败原样保留但对外标注可能已消费」处理，不自动重试。
//
// 聊天：POST https://api.cline.bot/api/v1/chat/completions（OpenAI 兼容）
//
//	Authorization: Bearer workos:<access>（Kind="oauth" 时必须带 workos: 前缀；
//	Kind="api" 时裸 Key）
//	+ ClientHeaders 里那一整套客户端指纹头，缺一个就可能 401/403。
//
// 两件 Java 侧独有的行为，本包只记录/提供工具，不接线：
//   - 非流式响应外面裹了一层 {"data":{…}}，要拆掉才是标准 OpenAI 形状，
//     见 UnwrapChatResponse；
//   - Cline 拒收客户端自带的 providerOptions 路由（HTTP 403），所以
//     ClineRoutingPolicy.apply 在 Java 里是空实现 —— 调用方也别往 payload 里塞路由。
package cline

import (
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

// 端点与客户端常量。全部照抄 ClineAuth.java 的硬编码，不要凭印象改。
const (
	// ProviderID 是网关里这一家的标识。
	ProviderID = "cline"

	// API 是 Cline 的接口根。
	API = "https://api.cline.bot/api/v1"

	// ChatPath 拼在 API 后面就是聊天端点（OpenAI 兼容）。
	ChatPath = "/chat/completions"
	// RegisterPath WorkOS 令牌换 Cline 令牌。
	RegisterPath = "/auth/register"
	// RefreshPath 续期。
	RefreshPath = "/auth/refresh"

	// RecommendedModelsPath 推荐模型列表（免登录）。
	RecommendedModelsPath = "/ai/cline/recommended-models"
	// ModelsPath 云端全量目录（免登录）。
	ModelsPath = "/ai/cline/models"

	// UsersMePath / BalancePathFmt / UsageLimitsPath 是用量面板的三个只读端点。
	UsersMePath     = "/users/me"
	BalancePathFmt  = "/users/%s/balance" // %s = userId（要 URL 转义）
	UsageLimitsPath = "/users/me/plan/usage-limits"

	// WorkOSClient 是 Cline CLI 在 WorkOS 上的公开 client_id。
	WorkOSClient = "client_01K3A541FN8TA3EPPHTD2325AR"
	// WorkOSDeviceURL 申请设备码。
	WorkOSDeviceURL = "https://api.workos.com/user_management/authorize/device"
	// WorkOSTokenURL 轮询设备码换令牌。
	WorkOSTokenURL = "https://api.workos.com/user_management/authenticate"
	// DeviceGrant 是 OAuth 设备流的 grant_type。
	DeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
	// WorkOSPrefix OAuth 账号的 access token 在 Cline 侧必须带这个前缀。
	WorkOSPrefix = "workos:"
	// RefreshGrantType 续期用的 grantType 字段值（放在 JSON 体里，不是表单）。
	RefreshGrantType = "refresh_token"

	// ClientType / ClientVersion / ClientPlatform / ClientCore 是一整套客户端指纹。
	ClientType     = "cline-cli"
	ClientVersion  = "3.0.68"
	ClientPlatform = "cli"
	ClientCore     = "0.0.90"

	// UserAgent 固定写成 Cline/<版本>。
	UserAgent = "Cline/" + ClientVersion
	// HTTPReferer / Title 是 Cline 侧要求来源标识的头。
	HTTPReferer = "https://cline.bot"
	Title       = "Cline"

	// ChatAccept 聊天请求的 Accept（要同时声明 SSE 和 JSON）。
	ChatAccept = "text/event-stream, application/json"

	// DefaultVerificationURI 是 WorkOS 没给 verification_uri_complete 时的兜底页。
	DefaultVerificationURI = "https://app.cline.bot"

	// RefreshLead 是请求前预检的提前量：只剩 5 分钟就先续期。
	// （上游插件另有 10 分钟的 RENEW_LEAD，那是交给宿主 proactive hook 的，
	// 请求内判据只有这一个 5 分钟，两个不要混。）
	RefreshLead = 5 * time.Minute

	// DefaultDeviceTTL 是设备码默认有效期。
	DefaultDeviceTTL = 300 * time.Second
	// DefaultInterval 是设备流默认轮询间隔。
	DefaultInterval = 5 * time.Second

	// connectTimeout / readTimeout 对齐 Java 侧 15s / 20s（register 是 20s/30s）。
	connectTimeout = 15 * time.Second
	readTimeout    = 20 * time.Second
)

// ErrPending 表示用户还没在浏览器里确认，继续轮询即可。
var ErrPending = errors.New("cline: 授权尚未完成（authorization_pending）")

// ErrSlowDown 表示轮询太快，上游要求放慢（WorkOS 的 slow_down）。
var ErrSlowDown = errors.New("cline: 轮询过快（slow_down）")

// ErrNoAccessToken 表示上游答复里没有可用的 access token。
var ErrNoAccessToken = errors.New("cline: 上游没有返回 access token")

// HTTPClient 是本包默认客户端（不设总超时，靠 ctx 控制）。
var HTTPClient = &http.Client{Timeout: 0}

// Account 是一个 Cline 账号的凭据。
//
// Kind 取值：
//   - "oauth"：WorkOS 设备流登录，聊天时必须给 access 加 workos: 前缀；
//   - "api"：app.cline.bot 的个人 API Key，裸 Bearer。
//
// Expires 是毫秒时间戳；未知时填 0（那就会一直用到 401 为止）。
type Account struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	Kind    string `json:"kind,omitempty"`
	UID     string `json:"uid,omitempty"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`
}

// ChatURL 是完整聊天端点。
func ChatURL() string { return API + ChatPath }

// ClientHeaders 返回 Cline 要求的一整套客户端指纹头（不含 Authorization）。
//
// taskID 可以空；非空时作为 X-Task-ID 带上。
func ClientHeaders(taskID string) map[string]string {
	h := map[string]string{
		"HTTP-Referer":       HTTPReferer,
		"X-Title":            Title,
		"User-Agent":         UserAgent,
		"X-IS-MULTIROOT":     "false",
		"X-CLIENT-TYPE":      ClientType,
		"X-CLIENT-VERSION":   ClientVersion,
		"X-PLATFORM":         ClientPlatform,
		"X-PLATFORM-VERSION": ClientVersion,
		"X-CORE-VERSION":     ClientCore,
	}
	if taskID != "" {
		h["X-Task-ID"] = taskID
	}
	return h
}

// Headers 返回发聊天请求要用的全部请求头（含 Authorization 与 Accept）。
//
// acc 为 nil 时只返回客户端指纹头（用于免登录的模型目录请求）。
func Headers(acc *Account) map[string]string {
	h := ClientHeaders("")
	if acc == nil {
		return h
	}
	h["Authorization"] = "Bearer " + bearer(acc)
	h["Accept"] = ChatAccept
	return h
}

// bearer 按账号类型给 access 加不加 workos: 前缀。
func bearer(acc *Account) string {
	tok := acc.Access
	if acc.Kind == "api" {
		return tok
	}
	if strings.HasPrefix(strings.ToLower(tok), WorkOSPrefix) {
		return tok
	}
	return WorkOSPrefix + tok
}

// UnwrapChatResponse 拆掉非流式聊天响应外面那层 {"data":{…}}。
//
// Cline 的 /chat/completions 把标准 OpenAI 对象裹在 data 里，且 usage 可能落在
// 外层。本函数把 data 提上来、顺手把外层 usage 补进 data（如果 data 自己没有）。
// 不是这种包装（直接就是 choices 形状）时原样返回。
func UnwrapChatResponse(body []byte) ([]byte, error) {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(body, &outer); err != nil {
		return body, nil // 不是 JSON 就原样交回去，不拦
	}
	rawData, ok := outer["data"]
	if !ok || string(rawData) == "null" {
		return body, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(rawData, &data); err != nil || data == nil {
		return body, nil
	}
	if _, hasChoices := data["choices"]; !hasChoices {
		return body, nil
	}
	if _, hasUsage := data["usage"]; !hasUsage {
		if u, ok := outer["usage"]; ok {
			data["usage"] = u
		}
	}
	out, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("cline: 重打包响应失败: %w", err)
	}
	return out, nil
}

// DeviceFlow 是一次进行中的 WorkOS 设备流登录。
type DeviceFlow struct {
	// DeviceCode 是轮询用的 device_code。
	DeviceCode string
	// UserCode 是用户要在授权页输入的短码。
	UserCode string
	// VerificationURI 授权页。
	VerificationURI string
	// VerificationURIComplete 带 user_code 的授权页（优先给用户这个）。
	VerificationURIComplete string
	// Deadline 设备码到期时间。
	Deadline time.Time
	// Interval 轮询间隔。
	Interval time.Duration
	// CreatedAt 发起时间。
	CreatedAt time.Time
}

// StartDeviceFlow 向 WorkOS 申请设备码。
//
// 请求是表单 POST，只有 client_id 一个参数（没有 PKCE）。
func StartDeviceFlow(ctx context.Context) (*DeviceFlow, error) {
	form := url.Values{}
	form.Set("client_id", WorkOSClient)
	status, body, err := doForm(ctx, WorkOSDeviceURL, form)
	if err != nil {
		return nil, fmt.Errorf("cline: 申请设备码失败: %w", err)
	}
	if status < 200 || status >= 400 {
		return nil, fmt.Errorf("cline: 申请设备码失败（HTTP %d）", status)
	}

	var j struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("cline: 设备码响应不是 JSON: %w", err)
	}
	if j.DeviceCode == "" {
		return nil, errors.New("cline: WorkOS 设备码响应不完整（无 device_code）")
	}

	f := &DeviceFlow{
		DeviceCode:              j.DeviceCode,
		UserCode:                j.UserCode,
		VerificationURI:         j.VerificationURI,
		VerificationURIComplete: j.VerificationURIComplete,
		CreatedAt:               time.Now(),
	}
	if f.VerificationURIComplete == "" {
		f.VerificationURIComplete = f.VerificationURI
	}
	if f.VerificationURIComplete == "" {
		f.VerificationURIComplete = DefaultVerificationURI
	}
	ttl := DefaultDeviceTTL
	if j.ExpiresIn > 0 {
		ttl = time.Duration(j.ExpiresIn) * time.Second
	}
	f.Deadline = f.CreatedAt.Add(ttl)
	secs := j.Interval
	if secs < 1 {
		secs = int64(DefaultInterval / time.Second)
	}
	f.Interval = time.Duration(secs) * time.Second
	return f, nil
}

// PollDeviceFlow 轮询一次授权结果。
//
// 返回：
//   - (acc, nil)         拿到账号（内部已完成 register 交换）
//   - (nil, ErrPending)  用户还没确认，隔 flow.Interval 再来
//   - (nil, ErrSlowDown) 上游要求放慢；照 Java 侧行为不自动改 Interval，
//     调用方可以自己加 5 秒
//   - (nil, err)         真失败
func PollDeviceFlow(ctx context.Context, flow *DeviceFlow) (*Account, error) {
	if flow == nil || flow.DeviceCode == "" {
		return nil, errors.New("cline: 登录流程已失效，请重新发起")
	}
	if !flow.Deadline.IsZero() && time.Now().After(flow.Deadline) {
		return nil, errors.New("cline: 登录码已过期，请重新发起")
	}

	form := url.Values{}
	form.Set("grant_type", DeviceGrant)
	form.Set("device_code", flow.DeviceCode)
	form.Set("client_id", WorkOSClient)
	status, body, err := doForm(ctx, WorkOSTokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("cline: 轮询授权失败: %w", err)
	}

	var j struct {
		Error        string `json:"error"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("cline: 轮询返回非 JSON（HTTP %d）", status)
	}
	switch j.Error {
	case "authorization_pending":
		return nil, ErrPending
	case "slow_down":
		return nil, ErrSlowDown
	case "":
		// 继续
	default:
		return nil, fmt.Errorf("cline: WorkOS 拒绝了授权（%s）", j.Error)
	}
	if j.AccessToken == "" {
		return nil, ErrNoAccessToken
	}

	return register(ctx, j.AccessToken, j.RefreshToken)
}

// register 用 WorkOS 令牌换 Cline 自己的令牌对。
func register(ctx context.Context, workosAccess, workosRefresh string) (*Account, error) {
	payload, err := json.Marshal(map[string]string{
		"accessToken":  workosAccess,
		"refreshToken": workosRefresh,
	})
	if err != nil {
		return nil, err
	}

	h := ClientHeaders("")
	h["Content-Type"] = "application/json"
	h["Accept"] = "application/json"

	status, resp, err := doJSON(ctx, API+RegisterPath, h, payload, 20*time.Second, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cline: 换 Cline 令牌失败: %w", err)
	}
	if status < 200 || status >= 400 {
		return nil, fmt.Errorf("cline: 换 Cline 令牌失败（HTTP %d）", status)
	}

	acc, err := parseAuth(resp, workosRefresh)
	if err != nil {
		return nil, err
	}
	acc.Kind = "oauth"
	return acc, nil
}

// RefreshToken 续期。成功后原地改写 acc 的 Access / Refresh / Expires。
//
// 一次性凭据注意：refresh token 只在请求体里出现（请求头、URL 都不带），
// 所以「请求体没开始写就失败」= 凭据没交付、可以重试；一旦请求体可能已发出，
// 上游可能已经消费掉这枚值，不要拿旧值重放，应当让用户重新登录。
// 本函数无法区分这两种情况，也不自动重试：失败时 acc 保持原值，由调用方定夺。
func RefreshToken(ctx context.Context, acc *Account) error {
	if acc == nil {
		return errors.New("cline: 账号为空")
	}
	if strings.TrimSpace(acc.Refresh) == "" {
		return errors.New("cline: 没有 refresh token，无法续期")
	}

	payload, err := json.Marshal(map[string]string{
		"refreshToken": acc.Refresh,
		"grantType":    RefreshGrantType,
	})
	if err != nil {
		return err
	}

	h := ClientHeaders("")
	h["Content-Type"] = "application/json"
	h["Accept"] = "application/json"

	status, resp, err := doJSON(ctx, API+RefreshPath, h, payload, connectTimeout, readTimeout)
	if err != nil {
		return fmt.Errorf("cline: 续期请求失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return &RefreshError{Status: status, Body: string(resp)}
	}

	next, err := parseAuth(resp, acc.Refresh)
	if err != nil {
		return err
	}
	acc.Access = next.Access
	acc.Refresh = next.Refresh
	acc.Expires = next.Expires
	if acc.Kind == "" {
		acc.Kind = "oauth"
	}
	if acc.UID == "" {
		acc.UID = next.UID
	}
	if acc.Email == "" {
		acc.Email = next.Email
	}
	return nil
}

// RefreshError 是一次续期被上游拒绝的答复。
type RefreshError struct {
	// Status 是上游 HTTP 状态码。
	Status int
	// Body 是上游正文。**可能含上游原话，不要直接回显给终端用户。**
	Body string
}

func (e *RefreshError) Error() string {
	return fmt.Sprintf("cline: 续期被拒（HTTP %d）", e.Status)
}

// SignInRequired 按 ClineFailurePolicy 的口径判定这次拒绝是否等于「登录失效」：
// 只有 400/401/403 且正文含 invalid|expired|revoked|unauthorized 才算，
// 其余（503/429/其他 4xx）都是「稍后可以拿同一枚 refresh token 再试」。
func (e *RefreshError) SignInRequired() bool {
	if e == nil {
		return false
	}
	if e.Status != 400 && e.Status != 401 && e.Status != 403 {
		return false
	}
	low := strings.ToLower(e.Body)
	for _, kw := range []string{"invalid", "expired", "revoked", "unauthorized"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

// NeedsRefresh 判断是否该在发请求前先续期（只剩不到 RefreshLead 就续）。
func NeedsRefresh(acc *Account, now time.Time) bool {
	if acc == nil || acc.Refresh == "" {
		return false
	}
	if acc.Expires <= 0 {
		return false
	}
	return acc.Expires-now.UnixMilli() <= RefreshLead.Milliseconds()
}

// userInfo 是两种嵌法（userInfo / user）共用的结构。
type userInfo struct {
	ClineUserID string `json:"clineUserId"`
	Subject     string `json:"subject"`
	ID          string `json:"id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
}

// parseAuth 解 Cline 的令牌响应。
//
// 兼容 camelCase 与 snake_case：data.accessToken/access_token、
// data.refreshToken/refresh_token、data.expiresAt、data.userInfo/user。
func parseAuth(body []byte, fallbackRefresh string) (*Account, error) {
	var wrap struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("cline: 令牌响应不是 JSON: %w", err)
	}
	data := body
	if len(wrap.Data) > 0 && string(wrap.Data) != "null" {
		data = wrap.Data
	}

	var d struct {
		AccessToken  string    `json:"accessToken"`
		AccessToken2 string    `json:"access_token"`
		RefreshToken string    `json:"refreshToken"`
		RefreshTok2  string    `json:"refresh_token"`
		ExpiresAt    string    `json:"expiresAt"`
		UserInfo     *userInfo `json:"userInfo"`
		User         *userInfo `json:"user"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("cline: 令牌响应结构不符: %w", err)
	}

	access := firstNonEmpty(d.AccessToken, d.AccessToken2)
	if access == "" {
		return nil, ErrNoAccessToken
	}

	acc := &Account{
		Access:  access,
		Refresh: firstNonEmpty(d.RefreshToken, d.RefreshTok2, fallbackRefresh),
		Expires: isoToMillis(d.ExpiresAt),
	}
	ui := d.UserInfo
	if ui == nil {
		ui = d.User
	}
	if ui != nil {
		acc.UID = firstNonEmpty(ui.ClineUserID, ui.Subject, ui.ID)
		acc.Email = ui.Email
		acc.Name = ui.Name
	}
	return acc, nil
}

// isoToMillis 解 Cline 的 expiresAt（形如 2026-11-14T09:30:00Z 或带小数秒）。
// 解析不出来时退回「一小时后」，与 Java 侧一致。
func isoToMillis(iso string) int64 {
	s := strings.TrimSpace(iso)
	if s == "" {
		return time.Now().Add(time.Hour).UnixMilli()
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		end := i
		for end < len(s) && (s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
			end++
		}
		s = s[:i] + s[end:]
	}
	s = strings.TrimSuffix(s, "Z")
	t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.UTC)
	if err != nil {
		return time.Now().Add(time.Hour).UnixMilli()
	}
	return t.UnixMilli()
}

// doForm 发一个表单 POST（照 Java 侧只给 Content-Type / Accept）。
func doForm(ctx context.Context, endpoint string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return send(req)
}

// doJSON 发一个 JSON POST（调用方已把 Authorization 等头放进 h）。
func doJSON(ctx context.Context, endpoint string, h map[string]string, body []byte,
	connTO, readTO time.Duration) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	return sendWith(req, connTO+readTO)
}

// send 用默认超时发请求。
func send(req *http.Request) (int, []byte, error) {
	return sendWith(req, connectTimeout+readTimeout)
}

// sendWith 发请求并读全正文（4xx/5xx 也读，方便上层定性）。
//
// 不用 http.Client.Timeout（会截断长响应），用 ctx 做整体上限就够。
func sendWith(req *http.Request, overall time.Duration) (int, []byte, error) {
	if overall <= 0 {
		overall = 35 * time.Second
	}
	ctx, cancel := context.WithTimeout(req.Context(), overall)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
