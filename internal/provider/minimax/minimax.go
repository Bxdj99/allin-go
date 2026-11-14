// Package minimax 封装 MiniMax Code（mcode）的认证链路、聊天协议与每日签到。
//
// 移植自 All-in 的 MiniMaxAuth.java / MiniMaxAllowance.java / MiniMaxLifecycle.java
// 以及 GatewayServer.forwardMiniMax 里那几条分支。
//
// 一、登录：MiniMax Code 自己的 OAuth 设备流（PKCE S256，公开客户端 mcode-public）
//
//  1. POST https://account.minimax.cn/oauth2/device/code   （form）
//     client_id=mcode-public
//     scope=agent.default
//     audience=agent-backend
//     code_challenge=<BASE64URL(SHA256(verifier))>（无 padding）
//     code_challenge_method=S256
//     → user_code / device_code / verification_uri / expires_in / interval
//     另有 MiniMax 自己的变体：不给 device_code、只给 user_code + expired_in，
//     这种时候轮询改用 user_code（本包用 DeviceFlow.PollByUser 标记）。
//  2. 用户在浏览器输 user_code 确认。
//  3. POST https://account.minimax.cn/oauth2/token            （form）
//     grant_type=urn:ietf:params:oauth:grant-type:device_code
//     device_code=…（byUser 变体是 user_code=…）
//     client_id=mcode-public&code_verifier=<verifier>
//     → access_token / refresh_token / expires_in；
//     未完成时 body 里 status=pending 或 error=authorization_pending。
//
// PKCE 细节：verifier = BASE64URL(32 字节随机)（无 = 补位），
// challenge = BASE64URL(SHA256(verifier 的 ASCII 字节))，method 固定 "S256"。
// 注意 verifier/challenge 都是 **无 padding 的 base64url**（Java 侧手写的 B64 表）。
//
// 二、续期：POST https://account.minimax.cn/oauth2/token（form）
//
//	grant_type=refresh_token&refresh_token=…&client_id=mcode-public
//	&scope=agent.default&audience=agent-backend
//
// refresh token 是一次性的：用一次就换新的，旧的立刻失效（第二次会 invalid_grant
// 把账号登出）。所以刷新成功后必须立刻落盘，并且同一账号的刷新要串行
// （MiniMaxLifecycle 那套 lease/generation 就是干这个的；本包只提供串行化钩子，
// 不做持久化）。
//
// 三、聊天：Anthropic Messages 形态
//
//	POST https://agent.minimax.cn/mavis/api/v1/llm/v1/messages
//	必需头：Authorization: Bearer <access>
//	        x-api-key: sk-xxx（占位常量，不是真 Key，但缺了不成）
//	        User-Agent: MiniMaxAgent
//	        X-Mavis-Agent-Id: main
//	        X-Mavis-Timezone-Offset: <本地时区偏移秒数>
//	        X-Mavis-Session-Id: <会话 id，缺了上游会自己生成>
//	        anthropic-version: 2023-06-01
//	        Accept: text/event-stream, application/json
//
// 四、签名账号 API（user/info、签到、额度）：host 换成 agent.minimaxi.com
// （注意不是聊天用的 agent.minimax.cn），且 Bearer 不够，必须另带
// yy / x-timestamp / x-signature 三个头 + mcode 客户端的一整套 query，
// 见 signedURL / applySignedHeaders 的注释。
package minimax

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 端点与常量。全部照抄 MiniMaxAuth.java 的硬编码，不要凭印象改。
const (
	// ProviderID 是网关里这一家的标识。
	ProviderID = "minimax-code"

	// Site / Lang 分别是国内站标识与语言。
	Site = "cn"
	Lang = "zh"

	// AccountURL 是 OAuth 账号域（设备流、续期都在这）。
	// 常量名不能叫 Account，会和下面的账号类型撞名。
	AccountURL = "https://account.minimax.cn"
	// LLM 是聊天域（国内）。国际版是 https://agent.minimax.io。
	LLM = "https://agent.minimax.cn"
	// Agent 是签名账号 API 的域（user/info、签到、额度），与聊天域不同。
	Agent = "https://agent.minimaxi.com"

	// APIPath 是聊天接口的公共前缀，拼上 /messages 才是聊天端点。
	APIPath = "/mavis/api/v1/llm/v1"
	// API 是完整的聊天前缀（= LLM + APIPath）。
	API = LLM + APIPath

	// DeviceCodePath 申请设备码。
	DeviceCodePath = "/oauth2/device/code"
	// TokenPath 设备码换令牌 / 续期共用。
	TokenPath = "/oauth2/token"

	// ModelsPath 模型目录（GET，可带可不带鉴权）。
	ModelsPath = "/mavis/api/v1/models"

	// UserInfoPath 签名接口：读账号真实 user id。
	UserInfoPath = "/v1/api/user/info"
	// SigninStatusPath 签名接口：一周签到面板。
	SigninStatusPath = "/minimax-cloud/api/v1/signin/status"
	// SigninClaimPath 签名接口：今天领取。
	SigninClaimPath = "/minimax-cloud/api/v1/signin/claim"
	// CreditDetailsPathFmt 签名接口：积分流水（%d 页、%d 页大小）。
	CreditDetailsPathFmt = "/minimax-cloud/api/v1/credit/details?page_number=%d&page_size=%d&include_expired=true"

	// ClientID 是 mcode 的公开客户端标识。
	ClientID = "mcode-public"
	// Scope / Audience 是设备流与续期都要带的两项。
	Scope    = "agent.default"
	Audience = "agent-backend"
	// DeviceGrant 是 OAuth 设备流的 grant_type。
	DeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
	// PKCEMethod 是唯一的 challenge 方法。
	PKCEMethod = "S256"

	// PlaceholderAPIKey 是聊天必带的占位 Key（真值就是这串，别改）。
	PlaceholderAPIKey = "sk-xxx"
	// ChatUserAgent 聊天请求的 UA。
	ChatUserAgent = "MiniMaxAgent"
	// MavisAgentID 聊天请求的 agent id。
	MavisAgentID = "main"
	// AnthropicVersion 聊天要带 anthropic-version。
	AnthropicVersion = "2023-06-01"
	// ChatAccept 聊天请求的 Accept。
	ChatAccept = "text/event-stream, application/json"

	// SignSecret 是签名账号 API 的密钥（来自 mcode 客户端二进制，插件常量）。
	SignSecret = "I*7Cf%WZ#S&%1RlZJ&C2"
	// SignUserAgent 签名接口的 UA。
	SignUserAgent = "MiniMaxCode"
	// SignGooeyTail 是 yy 签名末尾那个固定串。
	SignGooeyTail = "ooui"

	// CheckinQuery 是签到请求要带的桌面端形态 query（0.6.12）。
	CheckinQuery = "device_platform=web&is_desktop=1&desktop_version=0.6.12"

	// Lead 是提前续期的门槛：到期前 10 分钟就换（插件 LEAD_MS）。
	Lead = 10 * time.Minute

	// DefaultDeviceTTL 设备码默认有效期（expires_in 缺失时用）。
	DefaultDeviceTTL = 600 * time.Second
	// DefaultInterval 设备流默认轮询间隔。
	DefaultInterval = 5 * time.Second

	// connectTimeout / readTimeout 对齐 Java 侧 15s / 20s。
	connectTimeout = 15 * time.Second
	readTimeout    = 20 * time.Second
)

// 签到结果哨兵错误。
var (
	// ErrAlreadyChecked 今天已经领过了。
	ErrAlreadyChecked = errors.New("minimax: 今天已经签到过了")
	// ErrNotEligible 今天不可领（面板里今天那格是 status=4）。
	ErrNotEligible = errors.New("minimax: 今天不可领取")
	// ErrInactive 签到面板里没有「今天」这一格，或者今天还没开始（status 不是 2/3/4）。
	ErrInactive = errors.New("minimax: 今天不在签到周期内")
	// ErrNoUserID 读不到账号真实 realUserID。
	ErrNoUserID = errors.New("minimax: 读不到账号的 realUserID")
)

// 设备流轮询的两种「继续等」。
var (
	// ErrPending 用户还没确认。
	ErrPending = errors.New("minimax: 授权尚未完成")
	// ErrSlowDown 上游要求放慢；本函数会把 flow.Interval 加 5 秒。
	ErrSlowDown = errors.New("minimax: 轮询过快（slow_down）")
)

// HTTPClient 是本包默认客户端（不设总超时，靠 ctx 控制）。
var HTTPClient = &http.Client{Timeout: 0}

// refreshMu 让同进程内的续期串行，避免同一枚一次性 refresh token 被并发消费。
var refreshMu sync.Mutex

// Account 是一个 MiniMax Code 账号的凭据。
//
// Expires 是毫秒时间戳。SessionID 只影响聊天侧 X-Mavis-Session-Id（可留空）。
type Account struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh,omitempty"`
	Expires   int64  `json:"expires,omitempty"`
	UserID    string `json:"userID,omitempty"` // 签名接口要的 realUserID
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	GroupID   string `json:"groupID,omitempty"` // 可选，额度查询的 X-Group-Id
	SessionID string `json:"sessionID,omitempty"`
}

// ChatURL 是完整聊天端点（Anthropic Messages）。
func ChatURL() string { return API + "/messages" }

// ModelsURL 是模型目录端点。
func ModelsURL() string { return LLM + ModelsPath + "?region=" + Site + "&buildEnv=prod" }

// Headers 返回发聊天请求要用的全部请求头。
//
// sessionID 为空时按 acc.SessionID 来；acc.SessionID 也空就现场生成一个
// "magpie-<hex>"（与 Java 侧缺省值同形），保证每次请求都能拿到一个 id。
func Headers(acc *Account) map[string]string {
	h := map[string]string{
		"Accept":                  ChatAccept,
		"Content-Type":            "application/json",
		"x-api-key":               PlaceholderAPIKey,
		"User-Agent":              ChatUserAgent,
		"X-Mavis-Agent-Id":        MavisAgentID,
		"X-Mavis-Timezone-Offset": strconv.Itoa(timezoneOffsetSeconds(time.Now())),
		"anthropic-version":       AnthropicVersion,
	}
	if acc == nil {
		return h
	}
	h["Authorization"] = "Bearer " + acc.Access
	sid := acc.SessionID
	if sid == "" {
		sid = "magpie-" + strconv.FormatInt(time.Now().UnixNano()/1e6, 16)
	}
	h["X-Mavis-Session-Id"] = sid
	return h
}

// NeedsRefresh 判断是否该在发请求前先续期（只剩不到 Lead 就续）。
func NeedsRefresh(acc *Account, now time.Time) bool {
	if acc == nil || acc.Refresh == "" {
		return false
	}
	if acc.Expires <= 0 {
		return false
	}
	return acc.Expires-now.UnixMilli() <= Lead.Milliseconds()
}

// DeviceFlow 是一次进行中的 MiniMax 设备流登录。
type DeviceFlow struct {
	// FlowID 是轮询用的值：常规是 device_code，byUser 变体下等于 UserCode。
	FlowID string
	// UserCode 是用户要在授权页输入的短码。
	UserCode string
	// Verifier 是本次 PKCE 的 code_verifier（轮询要原样带回）。
	Verifier string
	// Challenge 是 code_challenge（= S256(Verifier)），留档方便核对。
	Challenge string
	// VerificationURI 授权页。
	VerificationURI string
	// VerificationURIComplete 带 user_code 的授权页（优先给用户这个）。
	VerificationURIComplete string
	// PollByUser 表示这次没有 device_code，轮询要用 user_code。
	PollByUser bool
	// Deadline 到期时间。
	Deadline time.Time
	// Interval 轮询间隔。
	Interval time.Duration
	// CreatedAt 发起时间。
	CreatedAt time.Time
}

// StartDeviceFlow 发起设备流（含 PKCE S256）。
func StartDeviceFlow(ctx context.Context) (*DeviceFlow, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("minimax: 生成 PKCE verifier 失败: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(seed)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("scope", Scope)
	form.Set("audience", Audience)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", PKCEMethod)

	status, body, err := doForm(ctx, AccountURL+DeviceCodePath, form)
	if err != nil {
		return nil, fmt.Errorf("minimax: 申请设备码失败: %w", err)
	}
	if status < 200 || status >= 400 {
		return nil, fmt.Errorf("minimax: 申请设备码失败（HTTP %d）", status)
	}

	var j struct {
		Error           string `json:"error"`
		UserCode        string `json:"user_code"`
		DeviceCode      string `json:"device_code"`
		VerificationURI string `json:"verification_uri"`
		VerificationURL string `json:"verification_url"`
		ExpiredIn       int64  `json:"expired_in"` // MiniMax 自己这个字段名，别当笔误
		ExpiresIn       int64  `json:"expires_in"`
		Interval        int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("minimax: 设备码响应不是 JSON（HTTP %d）", status)
	}
	if j.Error != "" {
		return nil, fmt.Errorf("minimax: 申请设备码被拒（%s）", j.Error)
	}

	uri := j.VerificationURI
	if uri == "" {
		uri = j.VerificationURL
	}
	byUser := j.DeviceCode == "" && j.UserCode != "" && j.ExpiredIn > 0
	if j.UserCode == "" || uri == "" || (j.DeviceCode == "" && !byUser) {
		return nil, errors.New("minimax: 设备码响应不完整")
	}

	f := &DeviceFlow{
		UserCode:        j.UserCode,
		Verifier:        verifier,
		Challenge:       challenge,
		PollByUser:      byUser,
		VerificationURI: uri,
		CreatedAt:       time.Now(),
	}
	if byUser {
		f.FlowID = j.UserCode
	} else {
		f.FlowID = j.DeviceCode
	}
	f.VerificationURIComplete = uri

	secs := j.ExpiresIn
	if secs <= 0 {
		secs = int64(DefaultDeviceTTL / time.Second)
	}
	f.Deadline = f.CreatedAt.Add(time.Duration(secs) * time.Second)

	interval := j.Interval
	if interval <= 0 {
		interval = int64(DefaultInterval / time.Second)
	}
	if byUser {
		// byUser 变体上游给的单位就是毫秒，这里照 Java 侧口径不乘 1000。
		f.Interval = time.Duration(interval) * time.Millisecond
	} else {
		f.Interval = time.Duration(interval) * time.Second
	}
	if f.Interval < time.Second {
		f.Interval = time.Second
	}
	return f, nil
}

// PollDeviceFlow 轮询一次授权结果。
//
// 返回：
//   - (acc, nil)         拿到账号
//   - (nil, ErrPending)  还没确认（网络抖动也按 pending 处理，与 Java 一致）
//   - (nil, ErrSlowDown) 要求放慢；本函数已把 flow.Interval 加 5 秒
//   - (nil, err)         真失败
func PollDeviceFlow(ctx context.Context, flow *DeviceFlow) (*Account, error) {
	if flow == nil || flow.FlowID == "" {
		return nil, errors.New("minimax: 登录流程已失效，请重新发起")
	}
	if !flow.Deadline.IsZero() && time.Now().After(flow.Deadline) {
		return nil, errors.New("minimax: 登录超时，请重新发起")
	}

	form := url.Values{}
	form.Set("grant_type", DeviceGrant)
	if flow.PollByUser {
		form.Set("user_code", flow.UserCode)
	} else {
		form.Set("device_code", flow.FlowID)
	}
	form.Set("client_id", ClientID)
	form.Set("code_verifier", flow.Verifier)

	status, body, err := doForm(ctx, AccountURL+TokenPath, form)
	if err != nil {
		return nil, ErrPending // 单次网络抖动不算失败
	}

	var j struct {
		Error        string `json:"error"`
		Status       string `json:"status"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		if status >= 400 && status != 408 && status != 429 && status < 500 {
			return nil, fmt.Errorf("minimax: 授权失败（HTTP %d）", status)
		}
		return nil, ErrPending
	}

	if j.AccessToken != "" && j.Error == "" {
		acc := &Account{
			Access:  j.AccessToken,
			Refresh: j.RefreshToken,
			Expires: expiresAt(j.ExpiresIn),
		}
		acc.Name = jwtClaim(j.AccessToken, "email")
		if acc.Name == "" {
			acc.Name = jwtClaim(j.AccessToken, "name")
		}
		acc.Email = jwtClaim(j.AccessToken, "email")
		acc.UserID = jwtClaim(j.AccessToken, "sub")
		return acc, nil
	}

	// 下面是「还没好 / 出事」的分流，照 pollOutcome 的口径。
	switch j.Error {
	case "authorization_pending":
		return nil, ErrPending
	case "slow_down":
		flow.Interval += 5 * time.Second
		return nil, ErrSlowDown
	case "access_denied":
		return nil, errors.New("minimax: 授权被拒绝")
	case "expired_token":
		return nil, errors.New("minimax: 登录码已过期，请重新发起")
	case "":
		// 看 status 字段
	default:
		return nil, fmt.Errorf("minimax: 授权失败（%s）", j.Error)
	}
	switch j.Status {
	case "pending", "authorization_pending":
		return nil, ErrPending
	case "slow_down":
		flow.Interval += 5 * time.Second
		return nil, ErrSlowDown
	case "access_denied", "denied":
		return nil, errors.New("minimax: 授权被拒绝")
	case "expired_token", "expired":
		return nil, errors.New("minimax: 登录码已过期，请重新发起")
	}
	if status == 408 || status == 429 || status >= 500 {
		return nil, ErrPending
	}
	if status >= 400 {
		return nil, fmt.Errorf("minimax: 授权失败（HTTP %d）", status)
	}
	return nil, errors.New("minimax: 授权响应不完整")
}

// RefreshToken 用 refresh token 换新 access，成功后原地改写 acc。
//
// 一次性凭据：refresh token 换过就作废，所以
//  1. 本函数内部用包级锁串行化，避免同进程并发消费同一枚值；
//  2. 返回 nil 时，调用方**必须立刻持久化** acc（含新的 Refresh）；
//  3. 失败时返回 error 且不动 acc；此时旧 refresh token 可能已被上游作废，
//     再试同一枚值多半会 invalid_grant，需要让用户重新登录。
func RefreshToken(ctx context.Context, acc *Account) error {
	if acc == nil {
		return errors.New("minimax: 账号为空")
	}
	rt := strings.TrimSpace(acc.Refresh)
	if rt == "" {
		return errors.New("minimax: 没有 refresh token，无法续期")
	}

	refreshMu.Lock()
	defer refreshMu.Unlock()

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt)
	form.Set("client_id", ClientID)
	form.Set("scope", Scope)
	form.Set("audience", Audience)

	status, body, err := doForm(ctx, AccountURL+TokenPath, form)
	if err != nil {
		return fmt.Errorf("minimax: 续期请求失败: %w", err)
	}

	var j struct {
		Error        string `json:"error"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return fmt.Errorf("minimax: 续期返回非 JSON（HTTP %d）", status)
	}
	if j.AccessToken == "" {
		if j.Error != "" {
			return fmt.Errorf("minimax: 续期被拒（%s）", j.Error)
		}
		return fmt.Errorf("minimax: 续期响应里没有 access_token（HTTP %d）", status)
	}

	acc.Access = j.AccessToken
	if j.RefreshToken != "" {
		acc.Refresh = j.RefreshToken
	}
	acc.Expires = expiresAt(j.ExpiresIn)
	return nil
}

// CheckinResult 是一次签到的完整结果。
type CheckinResult struct {
	// Outcome: claimed（本次领到）/ done（今天已领）/ ineligible（今天不可领）/
	// inactive（不在签到周期）
	Outcome string
	// Credit 是本次（或已领时）的积分数。
	Credit int
	// DayStatus 是面板里今天那格的 status（2 可领 / 3 已领 / 4 不可领）。
	DayStatus int
	// UserID 是签名接口用的 realUserID。
	UserID string
}

// Checkin 是每日签到，返回本次获得的积分数。
//
// 语义（与 Java 侧 checkin 一致）：
//   - 本次领取成功 → (积分, nil)
//   - 今天已经领过 → (那份积分, ErrAlreadyChecked)
//   - 今天不可领   → (0, ErrNotEligible)
//   - 不在周期内   → (0, ErrInactive)
//   - 其他失败     → (0, err)
//
// 需要区分「刚领到 / 早就领过」时用 CheckinFull。
func Checkin(ctx context.Context, acc *Account) (int, error) {
	res, err := CheckinFull(ctx, acc)
	if err != nil {
		return 0, err
	}
	switch res.Outcome {
	case "claimed":
		return res.Credit, nil
	case "done":
		return res.Credit, ErrAlreadyChecked
	case "ineligible":
		return 0, ErrNotEligible
	default:
		return 0, ErrInactive
	}
}

// CheckinFull 是签到主流程：读一周面板 → 今天可领就领一次。
//
// 面板判定（照 Java 侧）：data.days[] 里 is_today=true 的那格，
// status 2 = 可领、3 = 已领、4 = 不可领、其它一律当不在周期。
// 领取结果看 data.claim_result：1 = 本次领到、2 = 之前已领、其余算失败。
func CheckinFull(ctx context.Context, acc *Account) (*CheckinResult, error) {
	if acc == nil || acc.Access == "" {
		return nil, errors.New("minimax: 账号没有 access token")
	}
	uid := strings.TrimSpace(acc.UserID)
	if uid == "" {
		var err error
		uid, err = realUserID(ctx, acc)
		if err != nil {
			return nil, err
		}
		acc.UserID = uid
	}
	if uid == "" {
		return nil, ErrNoUserID
	}

	res := &CheckinResult{UserID: uid}

	status, body, err := signedGet(ctx, SigninStatusPath, acc, uid)
	if err != nil {
		return nil, fmt.Errorf("minimax: 读签到面板失败: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("minimax: 读签到面板失败（HTTP %d）", status)
	}

	var panel struct {
		Data struct {
			Days []struct {
				IsToday bool `json:"is_today"`
				Status  int  `json:"status"`
				Points  int  `json:"points"`
			} `json:"days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &panel); err != nil {
		return nil, fmt.Errorf("minimax: 签到面板结构不符: %w", err)
	}
	if len(panel.Data.Days) == 0 {
		return nil, errors.New("minimax: 签到面板里没有 days 字段")
	}

	var todayIdx = -1
	for i := range panel.Data.Days {
		if panel.Data.Days[i].IsToday {
			todayIdx = i
			break
		}
	}
	if todayIdx < 0 {
		res.Outcome = "inactive"
		return res, nil
	}
	today := &panel.Data.Days[todayIdx]

	res.DayStatus = today.Status
	switch today.Status {
	case 3:
		res.Outcome = "done"
		res.Credit = today.Points
		return res, nil
	case 4:
		res.Outcome = "ineligible"
		return res, nil
	case 2:
		// 可以领，往下走
	default:
		res.Outcome = "inactive"
		return res, nil
	}

	claimStatus, claimBody, err := signedPost(ctx, SigninClaimPath, acc, uid, "{}")
	if err != nil {
		return nil, fmt.Errorf("minimax: 领取失败: %w", err)
	}
	if claimStatus != 200 {
		return nil, fmt.Errorf("minimax: 领取失败（HTTP %d）", claimStatus)
	}

	var claim struct {
		Data struct {
			ClaimResult int `json:"claim_result"`
			Points      int `json:"points"`
		} `json:"data"`
	}
	if err := json.Unmarshal(claimBody, &claim); err != nil {
		return nil, fmt.Errorf("minimax: 领取返回结构不符: %w", err)
	}
	res.Credit = claim.Data.Points
	switch claim.Data.ClaimResult {
	case 1:
		res.Outcome = "claimed"
	case 2:
		res.Outcome = "done"
	default:
		return nil, fmt.Errorf("minimax: 未知 claim_result=%d", claim.Data.ClaimResult)
	}
	return res, nil
}

// realUserID 读账号真实 user id（签到必须以它作 user_id）。
func realUserID(ctx context.Context, acc *Account) (string, error) {
	status, body, err := signedGet(ctx, UserInfoPath, acc, "")
	if err != nil {
		return "", fmt.Errorf("minimax: 读账号信息失败: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("minimax: 读账号信息失败（HTTP %d）", status)
	}
	var j struct {
		Data struct {
			UserInfo struct {
				RealUserID  string `json:"realUserID"`
				RealUserID2 string `json:"real_user_id"`
			} `json:"userInfo"`
			UserInfo2 struct {
				RealUserID  string `json:"realUserID"`
				RealUserID2 string `json:"real_user_id"`
			} `json:"user_info"`
		} `json:"data"`
		UserInfo struct {
			RealUserID  string `json:"realUserID"`
			RealUserID2 string `json:"real_user_id"`
		} `json:"userInfo"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return "", fmt.Errorf("minimax: 账号信息结构不符: %w", err)
	}
	id := firstNonEmpty(
		j.Data.UserInfo.RealUserID, j.Data.UserInfo.RealUserID2,
		j.Data.UserInfo2.RealUserID, j.Data.UserInfo2.RealUserID2,
		j.UserInfo.RealUserID, j.UserInfo.RealUserID2,
	)
	if id == "" {
		return "", ErrNoUserID
	}
	return id, nil
}

// ===== 签名账号 API =====

// signedURL 拼出签名接口的完整 URL。
//
// 这一整套 query 是 mcode 客户端固定形态，缺项网关会拒；值一律不转义
// （Java 侧也是裸拼），user_id 为空时填 "0"。
func signedURL(path, userID string, now time.Time) string {
	ms := now.UnixMilli()
	tzSec := timezoneOffsetSeconds(now)

	base := path
	extra := ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		base, extra = path[:i], path[i+1:]
	}

	var q strings.Builder
	q.WriteString("device_platform=web")
	q.WriteString("&biz_id=3")
	q.WriteString("&app_id=3001")
	q.WriteString("&version_code=22201")
	q.WriteString("&is_desktop=1")
	q.WriteString("&desktop_version=0.6.12")
	q.WriteString("&unix=")
	q.WriteString(strconv.FormatInt(ms, 10))
	q.WriteString("&timezone_offset=")
	q.WriteString(strconv.Itoa(tzSec))
	q.WriteString("&sys_language=")
	q.WriteString(Lang)
	q.WriteString("&lang=")
	q.WriteString(Lang)
	q.WriteString("&device_id=0")
	q.WriteString("&os_name=android")
	q.WriteString("&browser_name=mcode")
	q.WriteString("&user_id=")
	if u := strings.TrimSpace(userID); u != "" {
		q.WriteString(u)
	} else {
		q.WriteString("0")
	}
	q.WriteString("&client=mcode")

	out := Agent + base + "?" + q.String()
	if extra != "" {
		out += "&" + extra
	}
	return out
}

// applySignedHeaders 给签名请求装上 Bearer 与三个签名头。
//
// 签名口径（逐字照 Java 侧 applySignedHeaders）：
//
//	pathWithQuery = URL 去掉 host 之后的整段（含 ?query）
//	body           = 请求体原文；GET 时签名用 "{}"，但 sig 走「无 body」分支
//	yy            = md5( encodeURIComponent(pathWithQuery) + "_" + body
//	                     + md5(<毫秒时间戳>) + "ooui" )
//	x-timestamp   = <秒级时间戳>
//	x-signature   = GET  ? md5( ts + SignSecret )
//	                POST ? md5( ts + SignSecret + body )
//
// 注意：body 必须与真正发出去的字节完全一致，签名后不要再重新序列化。
func applySignedHeaders(req *http.Request, pathWithQuery, access string, body *string, now time.Time) {
	ms := now.UnixMilli()
	ts := now.Unix() // 秒

	bodyForSign := "{}"
	if body != nil {
		bodyForSign = *body
	}
	yy := md5Hex(encodeURIComponent(pathWithQuery) + "_" + bodyForSign +
		md5Hex(strconv.FormatInt(ms, 10)) + SignGooeyTail)

	var sig string
	if body == nil {
		sig = md5Hex(strconv.FormatInt(ts, 10) + SignSecret)
	} else {
		sig = md5Hex(strconv.FormatInt(ts, 10) + SignSecret + *body)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", SignUserAgent)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("yy", yy)
	req.Header.Set("x-timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("x-signature", sig)
}

// signedGet 发签名 GET（不做重定向跟随）。
func signedGet(ctx context.Context, path string, acc *Account, userID string) (int, []byte, error) {
	now := time.Now()
	full := signedURL(path, userID, now)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return 0, nil, err
	}
	applySignedHeaders(req, full[len(Agent):], acc.Access, nil, now)
	return sendWith(req, connectTimeout+readTimeout)
}

// signedPost 发签名 POST（body 参与签名，发出去的就是签名的那个字符串）。
func signedPost(ctx context.Context, path string, acc *Account, userID, jsonBody string) (int, []byte, error) {
	if jsonBody == "" {
		jsonBody = "{}"
	}
	now := time.Now()
	full := signedURL(path, userID, now)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, strings.NewReader(jsonBody))
	if err != nil {
		return 0, nil, err
	}
	applySignedHeaders(req, full[len(Agent):], acc.Access, &jsonBody, now)
	return sendWith(req, connectTimeout+readTimeout)
}

// ===== 小工具 =====

// doForm 发表单 POST。
func doForm(ctx context.Context, endpoint string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return sendWith(req, connectTimeout+readTimeout)
}

// sendWith 发请求并读全正文（4xx/5xx 也读）。
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

// expiresAt 把 expires_in（秒）换成毫秒时间戳；缺失时按一小时算（与 Java 一致）。
func expiresAt(expiresIn int64) int64 {
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	}
	return time.Now().Add(time.Hour).UnixMilli()
}

// timezoneOffsetSeconds 返回本地时区在给定时刻的偏移秒数。
func timezoneOffsetSeconds(t time.Time) int {
	_, off := t.Zone()
	return off
}

// md5Hex 是 Java 侧 md5hex 的等价物（小写十六进制）。
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// encodeURIComponent 与 JS 的 encodeURIComponent 完全一致：
// 保留 A-Za-z0-9 - _ . ! ~ * ' ( )，其余按 UTF-8 逐字节 %XX（大写）。
func encodeURIComponent(s string) string {
	var sb strings.Builder
	const hexUpper = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
			sb.WriteByte(b)
		case b == '-' || b == '_' || b == '.' || b == '!' || b == '~' ||
			b == '*' || b == '\'' || b == '(' || b == ')':
			sb.WriteByte(b)
		default:
			sb.WriteByte('%')
			sb.WriteByte(hexUpper[b>>4])
			sb.WriteByte(hexUpper[b&0x0f])
		}
	}
	return sb.String()
}

// jwtClaim 取 JWT 里某个 claim（失败返回空串），只用于给账号起名。
func jwtClaim(token, key string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
