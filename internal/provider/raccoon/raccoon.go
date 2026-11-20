// Package raccoon 封装商汤「办公小浣熊」(xiaohuanxiong.com) 网页版的凭据与协议。
//
// 认证链路（移植自 All-in 的 RaccoonOAuthClient / RaccoonLoginProtocol /
// RaccoonAuthorizationFlow / RaccoonRefreshService）：
//
//  1. 浏览器授权：打开 https://xiaohuanxiong.com/code/authorize?login_source=desktop
//     &appname=办公小浣熊客户端&state=<随机>&desktop_callback=loopback&redirect=<回环地址>，
//     登录完成后回环地址收到 ?code=...（或被拒 ?error=...）。
//  2. 换 token：POST https://xiaohuanxiong.com/api/web/auth/v1/login_with_authorization_code
//     body 只有 {"authorization_code": code}，返回 data.access_token / data.refresh_token。
//     access_token 是 JWT，exp 就是到期时间，uid 从 sid/iss（hex）或 sub/id/user_id 取。
//  3. 续期：POST https://xiaohuanxiong.com/api/web/auth/v1/refresh，
//     body 只有 {"refresh_token": token}。**refresh_token 是一次性的**：POST 一旦可能
//     被上游处理过，就不许拿旧值重放（RaccoonRefreshSend 的整套阶段判定就是为此而写）。
//  4. 聊天：POST https://xiaohuanxiong.com/api/web/llm/v2/chat/completions，
//     就是 OpenAI 形态 + Authorization: Bearer <access>，没有额外信封。
//
// 注意：从浏览器 cookie 里扒出来的登录态（source=desktop）没有 refresh_token，
// 这种账号只能用到期为止，不能走续期分支。
package raccoon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 端点与常量（全部来自 Java 侧的硬编码，不要凭印象改）。
const (
	// ProviderID 是网关里这一家的标识。
	ProviderID = "raccoon"

	// WebOrigin 是小浣熊国内站根域，认证/聊天/目录都在它下面。
	WebOrigin = "https://xiaohuanxiong.com"

	// AuthBase 是授权码交换与续期的公共前缀。
	AuthBase = WebOrigin + "/api/web/auth/v1"
	// LoginPath 授权码换 token（POST）。
	LoginPath = "/api/web/auth/v1/login_with_authorization_code"
	// RefreshPath 一次性 refresh_token 换新 token（POST）。
	RefreshPath = "/api/web/auth/v1/refresh"

	// ChatURL 聊天补全（POST，OpenAI 兼容 body，SSE 响应）。
	ChatURL = WebOrigin + "/api/web/llm/v2/chat/completions"
	// CatalogURL 模型目录（GET）。
	CatalogURL = WebOrigin + "/api/web/llm/v2/model_catalog"

	// AuthorizeURL 浏览器授权页，参数见 AuthorizeURLFor。
	AuthorizeURL = WebOrigin + "/code/authorize"
	// CallbackPath 桌面端回环回调路径。
	CallbackPath = "/auth/callback"
	// AppName 授权页上显示的客户端名（原样带中文，不要转成英文）。
	//
	// 必须是上游登记的客户端标识。用错名字时授权页会回「授权链接无效，
	// 请从客户端重新发起登录」，而不是明确提示名字不对。
	// 注意：授权页「标题」显示的是「商汤小浣熊客户端」，但请求参数要用
	// 「办公小浣熊客户端」——显示名与参数值不是一回事，别按页面上看到的改。
	AppName = "办公小浣熊客户端"

	// EarlyRefreshMillis 提前量：到期前 5 分钟就该续期。
	EarlyRefreshMillis int64 = 5 * 60 * 1000

	// UserAgent 是聊天用的短 UA（很短，只有到 AppleWebKit/537.36 为止）。
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
	// CatalogUserAgent 是模型目录用的完整 Chrome UA，两者不能混用。
	CatalogUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// ChatAccept 聊天请求的 Accept。
	ChatAccept = "text/event-stream, application/json"
	// CatalogAcceptLanguage 目录请求带的语言。
	CatalogAcceptLanguage = "zh-Hans"

	// Timeout 单次非流式请求的超时（聊天流由调用方自己控制）。
	Timeout = 20 * time.Second
)

// 上游业务码。code 为 0 表示成功；下面几个是「登录态已失效」。
const (
	CodeOK = 0
	// CodeAuthCodeExpired 授权码过期（换 token 时才出现）。
	CodeAuthCodeExpired = 200035
)

// ExpiredCodes 是判定登录态失效的业务码集合。
var ExpiredCodes = []int{100002, 200001, 200003}

// HTTPClient 是包内共用的客户端；聊天流式请求建议调用方自建无总超时的 client。
var HTTPClient = &http.Client{Timeout: Timeout}

// Account 是一个小浣熊账号的凭据集合，字段名与配置里的 JSON 对齐。
type Account struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// UID 从 JWT 的 sid/iss（hex）或 sub/id/user_id 解出来。
	UID string `json:"uid,omitempty"`
	// Access 是 JWT access token。
	Access string `json:"access,omitempty"`
	// Token 与 Access 同值，兼容只读 token 字段的老配置。
	Token string `json:"token,omitempty"`
	// Refresh 是一次性 refresh token；浏览器 cookie 账号为空。
	Refresh string `json:"refresh,omitempty"`
	// Expires 是 access token 到期毫秒时间戳（0 = 未知）。
	Expires int64 `json:"expires,omitempty"`
	// Desktop 为 true 表示这条凭据来自浏览器 cookie，没有续期能力。
	Desktop bool `json:"desktop,omitempty"`
	// Source 正常是 browser 或 desktop。
	Source string `json:"source,omitempty"`
	// Type 一般填 oauth。
	Type string `json:"type,omitempty"`
	// AddedAt 入池时间（毫秒）。
	AddedAt int64 `json:"addedAt,omitempty"`
}

// Bearer 返回当前手上的 access token（Access 优先，回退 Token）。
func (a *Account) Bearer() string {
	if a == nil {
		return ""
	}
	if a.Access != "" {
		return strings.TrimSpace(strings.TrimPrefix(a.Access, "Bearer "))
	}
	return strings.TrimSpace(strings.TrimPrefix(a.Token, "Bearer "))
}

// IsDesktop 判断这是不是「只能用到过期」的浏览器 cookie 凭据。
func (a *Account) IsDesktop() bool {
	if a == nil {
		return false
	}
	return a.Desktop || strings.EqualFold(a.Source, "desktop")
}

// NeedsRefresh 判断是否该在 now 时刻续期（到期前 5 分钟内，且手里有 refresh）。
func (a *Account) NeedsRefresh(now time.Time) bool {
	if a == nil || a.IsDesktop() || a.Refresh == "" || a.Expires <= 0 {
		return false
	}
	return now.UnixMilli() >= a.Expires-EarlyRefreshMillis
}

// APIError 是上游的失败应答。
type APIError struct {
	Status int
	Code   int
	Msg    string
	// Expired 为 true 表示登录态已失效，需要重新登录或续期。
	Expired bool
}

func (e *APIError) Error() string {
	if e == nil {
		return "raccoon: <nil>"
	}
	if e.Code != 0 {
		return fmt.Sprintf("raccoon: 业务码 %d: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("raccoon: HTTP %d: %s", e.Status, e.Msg)
}

// Headers 返回聊天转发所需的请求头。
//
// 小浣熊只要你带 Bearer + 这两个头；缺 Accept 时上游不一定回 SSE。
func Headers(acc *Account) map[string]string {
	h := map[string]string{
		"User-Agent":   UserAgent,
		"Accept":       ChatAccept,
		"Content-Type": "application/json",
	}
	if tok := acc.Bearer(); tok != "" {
		h["Authorization"] = "Bearer " + tok
	}
	return h
}

// AuthorizeURLFor 拼出浏览器授权页地址。
//
// state 必须是本次登录随机生成的一次性值，回调时要原样比对；
// redirect 是本地回环地址（http://127.0.0.1:<端口>/auth/callback）。
func AuthorizeURLFor(state, redirect string) string {
	q := url.Values{}
	q.Set("login_source", "desktop")
	q.Set("appname", AppName)
	q.Set("state", state)
	q.Set("desktop_callback", "loopback")
	q.Set("redirect", redirect)
	return AuthorizeURL + "?" + q.Encode()
}

// ExchangeCode 用授权码换 access/refresh 这一对 token。
func ExchangeCode(ctx context.Context, code string) (*Account, error) {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 4096 {
		return nil, errors.New("raccoon: 授权码为空或过长")
	}
	body, _ := json.Marshal(map[string]string{"authorization_code": code})
	status, raw, err := postJSON(ctx, AuthBase+"/login_with_authorization_code", body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Details string `json:"details"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &resp)
	if resp.Code != nil && *resp.Code != CodeOK {
		expired := *resp.Code == CodeAuthCodeExpired
		return nil, &APIError{Status: status, Code: *resp.Code, Msg: pickText(resp.Details, resp.Message, resp.Msg, "上游业务请求失败"), Expired: expired}
	}
	if status < 200 || status >= 300 {
		return nil, &APIError{Status: status, Msg: pickText(resp.Details, resp.Message, resp.Msg, "HTTP "+itoa(status)), Expired: status >= 400 && status < 500}
	}
	access := cleanToken(resp.Data.AccessToken)
	refresh := cleanToken(resp.Data.RefreshToken)
	if access == "" || refresh == "" {
		return nil, errors.New("raccoon: 换 token 应答缺 access_token 或 refresh_token")
	}

	acc := &Account{
		Access:  access,
		Token:   access,
		Refresh: refresh,
		Source:  "browser",
		Type:    "oauth",
		AddedAt: time.Now().UnixMilli(),
		Expires: jwtExpiryMillis(access),
		Name:    jwtString(access, "name"),
		UID:     jwtUID(access),
	}
	acc.ID = firstNonEmpty(acc.Name, prefixOr(acc.UID, "Raccoon-"), "商汤小浣熊")
	return acc, nil
}

// RefreshToken 用一次性 refresh token 换一对新 token，成功后原地更新 acc。
//
// 上游有时不返回新的 refresh_token，此时保留旧值（与 Java 侧 parseRefresh 一致）。
// 一次性凭据的语义：本函数失败不代表可以重放，调用方必须自己决定是丢弃还是保留。
func RefreshToken(ctx context.Context, acc *Account) error {
	if acc == nil {
		return errors.New("raccoon: 账号为空")
	}
	if acc.IsDesktop() {
		return errors.New("raccoon: 浏览器 cookie 凭据没有续期能力")
	}
	refresh := cleanToken(acc.Refresh)
	if refresh == "" {
		return errors.New("raccoon: 缺少 refresh token")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": refresh})
	status, raw, err := postRefresh(ctx, body)
	if err != nil {
		return err
	}
	var resp struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Details string `json:"details"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
		Data struct {
			AccessToken  string          `json:"access_token"`
			RefreshToken string          `json:"refresh_token"`
			ExpiresIn    json.RawMessage `json:"expires_in"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &resp)
	if status < 200 || status >= 300 {
		return &APIError{Status: status, Msg: pickText(resp.Details, resp.Message, resp.Msg, resp.Error.Message, "HTTP "+itoa(status)), Expired: status >= 400 && status < 500}
	}
	if resp.Code != nil && *resp.Code != CodeOK {
		return &APIError{Status: status, Code: *resp.Code, Msg: pickText(resp.Details, resp.Message, resp.Msg, resp.Error.Message, "上游业务请求失败"), Expired: isExpiredCode(*resp.Code)}
	}
	access := cleanToken(resp.Data.AccessToken)
	if access == "" {
		return errors.New("raccoon: 续期应答没有 access_token")
	}
	next := cleanToken(resp.Data.RefreshToken)
	if next == "" {
		next = refresh
	}

	expires := expiryFromExpiresIn(resp.Data.ExpiresIn)
	if expires == 0 {
		expires = jwtExpiryMillis(access)
	}
	if expires == 0 {
		expires = acc.Expires
	}

	acc.Access = access
	acc.Token = access
	acc.Refresh = next
	acc.Expires = expires
	acc.Desktop = false
	return nil
}

// Models 拉真实模型目录（GET /api/web/llm/v2/model_catalog）。
//
// 目录结构是 data.categories[].models[]，取 name（缺了用 model_name）去重；
// 遇到 401 且账号可续期时续一次再试（对齐 GatewayServer.fetchRaccoonModels）。
func Models(ctx context.Context, acc *Account) ([]string, error) {
	tok := acc.Bearer()
	if tok == "" {
		return nil, errors.New("raccoon: 没有可用的 access token")
	}
	models, status, err := fetchCatalog(ctx, tok)
	if err == nil {
		return models, nil
	}
	var apiErr *APIError
	if status == http.StatusUnauthorized && errors.As(err, &apiErr) && !acc.IsDesktop() && acc.Refresh != "" {
		if refreshErr := RefreshToken(ctx, acc); refreshErr == nil {
			if next := acc.Bearer(); next != "" && next != tok {
				models, _, err = fetchCatalog(ctx, next)
				return models, err
			}
		}
	}
	return nil, err
}

func fetchCatalog(ctx context.Context, token string) ([]string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CatalogURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", CatalogUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", CatalogAcceptLanguage)

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("raccoon: 拉模型目录失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("raccoon: 读模型目录失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, &APIError{Status: resp.StatusCode, Msg: snippet(raw), Expired: resp.StatusCode == http.StatusUnauthorized}
	}

	var doc struct {
		Code *int `json:"code"`
		Data struct {
			Categories []struct {
				Models []struct {
					Name      string `json:"name"`
					ModelName string `json:"model_name"`
				} `json:"models"`
			} `json:"categories"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("raccoon: 模型目录格式无效: %w", err)
	}
	if doc.Code != nil && *doc.Code != CodeOK {
		return nil, resp.StatusCode, &APIError{Status: resp.StatusCode, Code: *doc.Code, Msg: "模型目录不可用", Expired: isExpiredCode(*doc.Code)}
	}
	if len(doc.Data.Categories) == 0 {
		return nil, resp.StatusCode, errors.New("raccoon: 模型目录为空")
	}

	seen := make(map[string]bool, 32)
	models := make([]string, 0, 32)
	for _, cat := range doc.Data.Categories {
		for _, m := range cat.Models {
			name := strings.TrimSpace(m.Name)
			if name == "" {
				name = strings.TrimSpace(m.ModelName)
			}
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			models = append(models, name)
		}
	}
	if len(models) == 0 {
		return nil, resp.StatusCode, errors.New("raccoon: 模型目录为空")
	}
	return models, resp.StatusCode, nil
}

// ===== 内部工具 =====

// postJSON 是换 token 的 POST：不跟随重定向、不缓存、限长读取。
func postJSON(ctx context.Context, endpoint string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Cache-Control", "no-store")
	return doRequest(req, "raccoon: 授权码交换失败")
}

// postRefresh 是续期的 POST：头集合与换 token 不同（多一个 Accept-Encoding: identity）。
func postRefresh(ctx context.Context, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, WebOrigin+RefreshPath, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Cache-Control", "no-store")
	return doRequest(req, "raccoon: 续期请求失败")
}

func doRequest(req *http.Request, what string) (int, []byte, error) {
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %w", what, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%s: 读应答失败: %w", what, err)
	}
	return resp.StatusCode, raw, nil
}

func isExpiredCode(code int) bool {
	for _, c := range ExpiredCodes {
		if c == code {
			return true
		}
	}
	return false
}

// cleanToken 去掉空白与可能残留的 "Bearer " 前缀。
func cleanToken(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "Bearer ")
	v = strings.TrimPrefix(v, "bearer ")
	return strings.TrimSpace(v)
}

// jwtClaims 解出 JWT 的 payload；解不出来返回 nil（不是错误，账号仍可用）。
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

// jwtExpiryMillis 把 exp 换成毫秒时间戳，取不到返回 0。
func jwtExpiryMillis(token string) int64 {
	claims := jwtClaims(token)
	if claims == nil {
		return 0
	}
	seconds, ok := toInt64(claims["exp"])
	if !ok || seconds <= 0 {
		return 0
	}
	return seconds * 1000
}

func jwtString(token, key string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	if v, ok := claims[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// jwtUID 先看 sid/iss（上游给的是 hex，要转十进制），再退回 sub/id/user_id。
func jwtUID(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	for _, key := range []string{"sid", "iss"} {
		v, ok := claims[key].(string)
		if !ok {
			continue
		}
		hex := strings.TrimSpace(v)
		hex = strings.TrimPrefix(hex, "0x")
		hex = strings.TrimPrefix(hex, "0X")
		if hex == "" || !isHex(hex) {
			continue
		}
		if n, ok := new(big.Int).SetString(hex, 16); ok {
			return n.String()
		}
	}
	for _, key := range []string{"sub", "id", "user_id"} {
		if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// expiryFromExpiresIn 解析 expires_in（秒），取不到返回 0。
func expiryFromExpiresIn(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	seconds, err := n.Int64()
	if err != nil || seconds <= 0 {
		return 0
	}
	now := time.Now().UnixMilli()
	if seconds > (1<<62)/1000 {
		return 0
	}
	return now + seconds*1000
}

func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case string:
		var n json.Number = json.Number(strings.TrimSpace(t))
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func pickText(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func prefixOr(v, prefix string) string {
	if s := strings.TrimSpace(v); s != "" {
		return prefix + s
	}
	return ""
}

func itoa(v int) string {
	var b [20]byte
	i := len(b)
	neg := v < 0
	if neg {
		v = -v
	}
	for {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
