package workbuddy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 登录相关端点（相对 origin）。
const (
	PathAuthState = "/v2/plugin/auth/state"
	PathAuthToken = "/v2/plugin/auth/token"
	PathAccount   = "/v2/plugin/login/account"
	PathAccounts  = "/v2/plugin/accounts"
)

// LoginFlow 是一次进行中的 WorkBuddy 登录。
//
// 与 Trae 不同，这家不需要本地回调服务器：先向官方要一个 state + authUrl，
// 用户在浏览器里登录后，用 state 轮询换 token。
type LoginFlow struct {
	Provider  string    `json:"provider"`
	State     string    `json:"state"`
	AuthURL   string    `json:"authUrl"`
	Deadline  time.Time `json:"deadline"`

	polled bool
}

// LoginAccount 是登录成功后拿到的账号。
type LoginAccount struct {
	UID            string `json:"uid"`
	Name           string `json:"name"`
	Access         string `json:"access"`
	Refresh        string `json:"refresh,omitempty"`
	Domain         string `json:"domain"`
	EnterpriseID   string `json:"enterpriseId,omitempty"`
	EnterpriseName string `json:"enterpriseName,omitempty"`
	Email          string `json:"email,omitempty"`
	Expires        int64  `json:"expires,omitempty"`
}

// originOf 返回供应商对应的域名。
func originOf(provider string) string {
	switch provider {
	case "workbuddy-ai":
		return "https://www.workbuddy.ai"
	default:
		return "https://copilot.tencent.com"
	}
}

// StartLogin 向官方申请一个登录态，返回需要用户打开的授权地址。
func StartLogin(ctx context.Context, provider string) (*LoginFlow, error) {
	origin := originOf(provider)
	endpoint := origin + PathAuthState + "?platform=" + url.QueryEscape(provider)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	// 这四个头告诉服务端「我还没有身份」，是官方插件首次登录时的形态
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-Department-Info", "true")
	req.Header.Set("User-Agent", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("申请登录态失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("响应解析失败（HTTP %d）", resp.StatusCode)
	}
	if env.Code != 0 || env.Data.State == "" {
		return nil, fmt.Errorf("申请登录态被拒: %s", env.Msg)
	}

	authURL := env.Data.AuthURL
	// 官方地址需要带上 version 与 loginSessionId（复刻桌面插件行为）
	if u, perr := url.Parse(authURL); perr == nil {
		q := u.Query()
		q.Set("version", "5.9.3")
		q.Set("loginSessionId", strings.ReplaceAll(newUUIDLike(), "-", ""))
		u.RawQuery = q.Encode()
		authURL = u.String()
	}

	return &LoginFlow{
		Provider: provider,
		State:    env.Data.State,
		AuthURL:  authURL,
		Deadline: time.Now().Add(5 * time.Minute),
	}, nil
}

// PollLogin 用 state 换 token 并取账号信息。未完成时返回 ErrPending。
func PollLogin(ctx context.Context, flow *LoginFlow) (*LoginAccount, error) {
	if flow == nil {
		return nil, fmt.Errorf("登录流为空")
	}
	if time.Now().After(flow.Deadline) {
		return nil, fmt.Errorf("登录超时，请重新发起")
	}
	origin := originOf(flow.Provider)
	q := "?state=" + url.QueryEscape(flow.State)

	// 1) 换 token
	token, err := callJSON(ctx, http.MethodGet, origin+PathAuthToken+q, nil, nil)
	if err != nil {
		return nil, err
	}
	var tEnv struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}
	if err := json.Unmarshal(token, &tEnv); err != nil {
		return nil, fmt.Errorf("token 响应解析失败")
	}
	access := extractAccess(token, tEnv.Data)
	if access == "" {
		// 还没授权完成时服务端通常回非 0 或空 data
		return nil, ErrPending
	}

	// 2) 取账号身份（只有 /login/account 才是权威来源，不看昵称猜）
	hdr := map[string]string{
		"Authorization":       "Bearer " + access,
		"X-Domain":            hostOf(flow.Provider),
		"X-No-User-Id":        "true",
		"X-No-Enterprise-Id":  "true",
	}
	who, err := callJSON(ctx, http.MethodGet, origin+PathAccount+q, hdr, nil)
	if err != nil {
		return nil, err
	}
	var wEnv struct {
		Code int `json:"code"`
		Data struct {
			UID            string `json:"uid"`
			Nickname       string `json:"nickname"`
			PhoneNumber    string `json:"phoneNumber"`
			Email          string `json:"email"`
			Type           string `json:"type"`
			EnterpriseID   string `json:"enterpriseId"`
			EnterpriseName string `json:"enterpriseName"`
		} `json:"data"`
	}
	if err := json.Unmarshal(who, &wEnv); err != nil {
		return nil, fmt.Errorf("账号信息解析失败")
	}
	uid := strings.TrimSpace(wEnv.Data.UID)
	if uid == "" {
		return nil, fmt.Errorf("服务端没有返回可验证的账号身份（uid 为空）")
	}

	name := wEnv.Data.Nickname
	if name == "" {
		name = wEnv.Data.PhoneNumber
	}
	if name == "" {
		name = uid
	}
	ent := strings.TrimSpace(wEnv.Data.EnterpriseID)
	if ent != "" {
		entName := wEnv.Data.EnterpriseName
		if entName == "" {
			tail := ent
			if len(tail) > 6 {
				tail = tail[len(tail)-6:]
			}
			entName = "团队 " + tail
		}
		name += " · " + entName
	}

	flow.polled = true
	return &LoginAccount{
		UID:            uid,
		Name:           name,
		Access:         access,
		Refresh:        extractRefresh(token), // 之前漏了，导致账号无法自动续期
		Domain:         hostOf(flow.Provider),
		EnterpriseID:   ent,
		EnterpriseName: wEnv.Data.EnterpriseName,
		Email:          wEnv.Data.Email,
		Expires:        jwtExpiryMillis(access),
	}, nil
}

// ErrPending 表示用户还没在浏览器里完成授权。
var ErrPending = fmt.Errorf("等待授权")

func hostOf(provider string) string {
	if provider == "workbuddy-ai" {
		return "www.workbuddy.ai"
	}
	return "copilot.tencent.com"
}

// callJSON 发一个请求并返回响应体。
func callJSON(ctx context.Context, method, endpoint string, headers map[string]string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("授权状态无效（HTTP %d）", resp.StatusCode)
	}
	return raw, nil
}

// extractAccess 从各种可能的响应形状里挖出 access token。
// extractRefresh 从 token 响应里取 refresh token。
//
// 上游会下发 refreshToken（或 refresh_token），但之前只提取了 access，
// refresh 被丢弃 —— 后果是账号永远没有续期能力，只能等着 401 后重新登录。
func extractRefresh(raw []byte) string {
	var outer struct {
		Refresh      string `json:"refresh"`
		RefreshToken string `json:"refreshToken"`
		Data         struct {
			Refresh      string `json:"refresh"`
			RefreshToken string `json:"refreshToken"`
			RefreshTk    string `json:"refresh_token"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &outer) != nil {
		return ""
	}
	for _, cand := range []string{
		outer.Data.Refresh, outer.Data.RefreshToken, outer.Data.RefreshTk,
		outer.Refresh, outer.RefreshToken,
	} {
		if v := strings.TrimSpace(cand); v != "" {
			return v
		}
	}
	return ""
}

// extractRefreshExpires 取 refresh token 自己的过期时间（毫秒）。
func extractRefreshExpires(raw []byte) int64 {
	var outer struct {
		RefreshExpiresAt int64 `json:"refreshExpiresAt"`
		RefreshExpiresIn int64 `json:"refreshExpiresIn"`
		Data             struct {
			RefreshExpiresAt int64 `json:"refreshExpiresAt"`
			RefreshExpiresIn int64 `json:"refreshExpiresIn"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &outer) != nil {
		return 0
	}
	now := time.Now().UnixMilli()
	if v := outer.Data.RefreshExpiresAt; v > 0 {
		return v
	}
	if v := outer.Data.RefreshExpiresIn; v > 0 {
		return now + v*1000
	}
	if v := outer.RefreshExpiresAt; v > 0 {
		return v
	}
	if v := outer.RefreshExpiresIn; v > 0 {
		return now + v*1000
	}
	return 0
}

func extractAccess(raw []byte, data any) string {
	// 先试 data 是对象
	var outer struct {
		Access     string `json:"access"`
		AccessToken string `json:"accessToken"`
		Token      string `json:"token"`
		Data       struct {
			Access      string `json:"access"`
			AccessToken string `json:"accessToken"`
			Token       string `json:"token"`
			Ck          string `json:"ck"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &outer) == nil {
		for _, cand := range []string{
			outer.Data.Access, outer.Data.AccessToken, outer.Data.Token, outer.Data.Ck,
			outer.Access, outer.AccessToken, outer.Token,
		} {
			if strings.TrimSpace(cand) != "" {
				return strings.TrimSpace(cand)
			}
		}
	}
	// data 直接是字符串的情况
	if s, ok := data.(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return ""
}

// newUUIDLike 生成带连字符的随机串。
func newUUIDLike() string {
	const hex = "0123456789abcdef"
	b := make([]byte, 32)
	seed := time.Now().UnixNano()
	for i := range b {
		b[i] = hex[(seed>>uint(i%16*4))&0xf]
	}
	s := string(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// HTTPClient 由 provider 包在 init 时注入带内置 CA 的 Transport。
var HTTPClient = &http.Client{Timeout: 30 * time.Second, Transport: http.DefaultTransport}

// jwtExpiryMillis 从 JWT 的 exp 解出过期时间（毫秒）；解不出返回 0。
//
// 上游不一定在响应里给 expires，但 access token 本身是 JWT，
// 取 exp 就能知道什么时候该刷新。
func jwtExpiryMillis(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	p := parts[1]
	p = strings.ReplaceAll(p, "-", "+")
	p = strings.ReplaceAll(p, "_", "/")
	for len(p)%4 != 0 {
		p += "="
	}
	dec, err := base64.StdEncoding.DecodeString(p)
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(dec, &claims) != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp * 1000
}
