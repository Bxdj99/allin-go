// Package loomy 封装讯飞「Loomy」(loomyad.xunfei.cn) 的登录态与协议。
//
// 认证链路（移植自 All-in 的 LoomyAuth）：
//
//  1. 发短信：POST https://account.xfinfr.com/login/phone/sendMsgCode
//     param = {"ccode":"86","phone":"1xxxxxxxxxx","expire":300}，返回 data.msgid。
//  2. 校验码：POST https://account.xfinfr.com/login/phone/checkCode
//     param = {"ccode","phone","mcode":6位数字,"msgid","expire":1209600}，
//     返回 data.session —— 这个 session 就是之后所有网关请求的凭据（约 14 天）。
//  3. 业务请求：全部打到 https://loomyad.xunfei.cn，带两个头
//     token: <session> 和 Authorization: Bearer <session>。
//  4. 聊天：POST /api/v1/chat/completions（OpenAI 形态，SSE）。
//
// 账号主机这一侧没有「普通的 Bearer 鉴权」，而是自签：
//
//	body      = {"base":{...},"param":{...}}
//	contentMD5= base64(md5(body))
//	canonical = "POST\n" + path + "\n\n" + contentMD5 + "\n" + "application/json\n"
//	          + date + "\n" + nonce + "\n\n"
//	Authorization: account <AK>:<base64(hmac_sha1(SK, canonical))>
//
// 注意：Loomy 这一家**没有 token 刷新**。session 过期（业务码 020002 / 100002 或
// HTTP 401/403）只能重新走短信登录，`RefreshDaily` 只是领每日积分，不是续期。
package loomy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// 端点与常量（全部来自 Java 侧 LoomyAuth 的硬编码）。
const (
	// ProviderID 是网关里这一家的标识。
	ProviderID = "loomy"

	// AccountBase 是账号主机，负责短信验证码与 session 签发。
	AccountBase = "https://account.xfinfr.com"
	// GatewayBase 是业务网关，聊天/模型/积分/绘图都在它下面。
	GatewayBase = "https://loomyad.xunfei.cn"

	// SendMsgCodePath 发短信验证码。
	SendMsgCodePath = "/login/phone/sendMsgCode"
	// CheckCodePath 用验证码换 session。
	CheckCodePath = "/login/phone/checkCode"

	// ChatURL 聊天补全（POST，OpenAI 兼容 body）。
	ChatURL = GatewayBase + "/api/v1/chat/completions"
	// ModelsURL 模型目录（GET）。
	ModelsURL = GatewayBase + "/api/v1/models"
	// ImagesURL 文生图（POST）。
	ImagesURL = GatewayBase + "/api/v1/images/generations"

	// RecordsPath 积分流水（GET，带死参数）。
	RecordsPath = "/api/v2/points/records?pageNo=1&pageSize=1&direction=debit&granularity=chat"
	// FirstLoginPath 每日首次登录领积分（POST，body 是 {}）。
	FirstLoginPath = "/api/v1/points/first-login"

	// AppID / ModelID / AppVersion / DeviceID / ClientUA 是桌面客户端的公共指纹，
	// 不是某个用户的凭据，写死在客户端里。
	AppID      = "GM3LOOMY"
	ModelID    = "Web"
	AppVersion = "1.0.0"
	DeviceID   = "web"
	ClientUA   = "Loomy|Desktop|Electron|macOS"

	// AK / SK 是桌面客户端内嵌的签名密钥对，签到接口用它做 HMAC 签名。
	AK = "2thryby66wxi53sk"
	SK = "zsak6eadrbawz683wf5r3m2snrwj868r"

	// CodeOK 是业务成功码（是字符串，不是数字）。
	CodeOK = "000000"
	// SendCodeExpire 短信验证码有效期（秒）。
	SendCodeExpire = 300
	// SessionExpire session 有效期（秒，14 天）。
	SessionExpire = 1209600

	// Timeout 单次非流式请求的超时。
	Timeout = 20 * time.Second
)

// ExpiredCodes 是判定 session 失效的业务码。
var ExpiredCodes = []string{"020002", "100002"}

// HTTPClient 是包内共用的客户端；聊天流式请求建议调用方自建无总超时的 client。
var HTTPClient = &http.Client{Timeout: Timeout}

// dateLayout 是签名里 Date 头的格式（GMT，英文月份缩写）。
const dateLayout = "Mon, 02 Jan 2006 15:04:05 GMT"

// Account 是一个 Loomy 账号的凭据集合。
type Account struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// Phone 是 11 位手机号（不带国家码）。
	Phone string `json:"phone,omitempty"`
	// Token 存登录拿到的 session，聊天与所有网关请求都用它。
	Token string `json:"token,omitempty"`
	// ExpiresAt 是 session 到期毫秒时间戳，0 表示未知。
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// AddedAt 入池时间（毫秒）。
	AddedAt int64 `json:"addedAt,omitempty"`
}

// Session 返回当前 session（去掉可能的 "Bearer " 前缀）。
func (a *Account) Session() string {
	if a == nil {
		return ""
	}
	v := strings.TrimSpace(a.Token)
	v = strings.TrimPrefix(v, "Bearer ")
	return strings.TrimSpace(v)
}

// Expired 判断 session 是否已过期（到期时间未知时按未过期处理）。
func (a *Account) Expired(now time.Time) bool {
	if a == nil || a.ExpiresAt <= 0 {
		return false
	}
	return now.UnixMilli() >= a.ExpiresAt
}

// APIError 是上游的失败应答。
type APIError struct {
	Status int
	Code   string
	Msg    string
	// Expired 为 true 表示 session 已失效，只能重新短信登录。
	Expired bool
}

func (e *APIError) Error() string {
	if e == nil {
		return "loomy: <nil>"
	}
	if e.Code != "" {
		return fmt.Sprintf("loomy: 业务码 %s: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("loomy: HTTP %d: %s", e.Status, e.Msg)
}

// Wallets 是积分余额快照，缺项为 nil。
type Wallets struct {
	Permanent      *float64 `json:"permanent,omitempty"`
	Daily          *float64 `json:"daily,omitempty"`
	DailyLimit     *float64 `json:"dailyLimit,omitempty"`
	DailyRemaining *float64 `json:"dailyRemaining,omitempty"`
}

// Headers 返回聊天转发所需的请求头。
//
// 关键点是 token 与 Authorization 必须同时存在：上游聊天端点只认 token，
// 而目录/积分接口只认 Bearer，两套都带上最省事。
func Headers(acc *Account) map[string]string {
	h := map[string]string{
		"Content-Type": "application/json;charset=utf-8",
		"Accept":       "text/event-stream",
	}
	if s := acc.Session(); s != "" {
		h["token"] = s
		h["Authorization"] = "Bearer " + s
	}
	return h
}

// Models 拉官方模型目录（GET /api/v1/models）。
//
// 应答是 {"code":"000000","data":[{"id":...}]}；data 为空或格式不对算失败，
// 不返回半份目录。
func Models(ctx context.Context, acc *Account) ([]string, error) {
	session := acc.Session()
	if session == "" {
		return nil, errors.New("loomy: 没有可用的 session")
	}
	status, raw, err := gatewayRequest(ctx, http.MethodGet, "/api/v1/models", session, nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Code    string          `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("loomy: 模型目录格式无效: %w", err)
	}
	if doc.Code != "" && doc.Code != CodeOK {
		return nil, &APIError{Status: status, Code: doc.Code, Msg: pickText(doc.Message, doc.Msg, "上游业务请求失败"), Expired: isExpiredCode(doc.Code)}
	}
	var entries []struct {
		ID string `json:"id"`
	}
	if len(doc.Data) == 0 || json.Unmarshal(doc.Data, &entries) != nil || len(entries) == 0 {
		return nil, errors.New("loomy: 模型目录为空或格式无效")
	}
	seen := make(map[string]bool, len(entries))
	models := make([]string, 0, len(entries))
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	if len(models) == 0 {
		return nil, errors.New("loomy: 模型目录为空")
	}
	return models, nil
}

// SendCode 发短信验证码，返回上游给的 msgid（校验码时必须带回去）。
func SendCode(ctx context.Context, phone string) (string, error) {
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return "", err
	}
	param := map[string]any{
		"ccode":  "86",
		"phone":  normalized,
		"expire": SendCodeExpire,
	}
	raw, err := signedPost(ctx, SendMsgCodePath, param)
	if err != nil {
		return "", err
	}
	data := dataOf(raw)
	id := strings.TrimSpace(stringField(data, "msgid"))
	if id == "" {
		return "", errors.New("loomy: 上游未返回验证码标识")
	}
	return id, nil
}

// CheckCode 用验证码换 session。msgid 是 SendCode 的返回值。
func CheckCode(ctx context.Context, phone, code, msgid string) (string, error) {
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return "", err
	}
	if !phoneCodePattern.MatchString(strings.TrimSpace(code)) {
		return "", errors.New("loomy: 验证码必须是 6 位数字")
	}
	if strings.TrimSpace(msgid) == "" {
		return "", errors.New("loomy: 缺少验证码标识")
	}
	param := map[string]any{
		"ccode":  "86",
		"phone":  normalized,
		"mcode":  strings.TrimSpace(code),
		"msgid":  strings.TrimSpace(msgid),
		"expire": SessionExpire,
	}
	raw, err := signedPost(ctx, CheckCodePath, param)
	if err != nil {
		return "", err
	}
	session := strings.TrimSpace(stringField(dataOf(raw), "session"))
	if session == "" {
		return "", errors.New("loomy: 上游未返回 session")
	}
	return session, nil
}

// LoginByCode 是 SendCode + CheckCode 的便捷封装，成功后返回填好凭据的账号。
func LoginByCode(ctx context.Context, phone, code string) (*Account, error) {
	msgid, err := SendCode(ctx, phone)
	if err != nil {
		return nil, err
	}
	session, err := CheckCode(ctx, phone, code, msgid)
	if err != nil {
		return nil, err
	}
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &Account{
		ID:        normalized,
		Phone:     normalized,
		Token:     session,
		ExpiresAt: now.Add(SessionExpire * time.Second).UnixMilli(),
		AddedAt:   now.UnixMilli(),
	}, nil
}

// Usage 读积分余额（GET /api/v2/points/records，读操作，可以用它验证 session 是否还行）。
func Usage(ctx context.Context, acc *Account) (*Wallets, error) {
	session := acc.Session()
	if session == "" {
		return nil, errors.New("loomy: 没有可用的 session")
	}
	status, raw, err := gatewayRequest(ctx, http.MethodGet, RecordsPath, session, nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Code    string          `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("loomy: 积分应答格式无效: %w", err)
	}
	if doc.Code != "" && doc.Code != CodeOK {
		return nil, &APIError{Status: status, Code: doc.Code, Msg: pickText(doc.Message, doc.Msg, "上游业务请求失败"), Expired: isExpiredCode(doc.Code)}
	}
	data := unwrap(doc.Data)
	w := &Wallets{
		Permanent:      numberField(data, "balance"),
		Daily:          numberField(data, "dailyBalance"),
		DailyLimit:     numberField(data, "dailyLimitPoints"),
		DailyRemaining: numberField(data, "dailyRemainingPoints"),
	}
	if w.Permanent == nil && w.Daily == nil && (w.DailyLimit == nil || w.DailyRemaining == nil) {
		return nil, errors.New("loomy: 积分应答缺少余额")
	}
	return w, nil
}

// RefreshDaily 领每日首次登录积分。
//
// 顺序是不能反的：先读一次余额（读失败就不动写操作），再打 first-login，
// 否则会在「读都读不到」的情况下平白发一次写请求。
// outcome 取值：claimed（涨了）/ done（没涨，今天已领）/ unknown（对不上）。
func RefreshDaily(ctx context.Context, acc *Account) (outcome string, before, after *float64, err error) {
	beforeWallets, err := Usage(ctx, acc)
	if err != nil {
		return "", nil, nil, err
	}
	before = beforeWallets.Daily

	status, raw, err := gatewayRequest(ctx, http.MethodPost, FirstLoginPath, acc.Session(), []byte("{}"))
	if err != nil {
		return "", before, nil, err
	}
	var doc struct {
		Code    string          `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", before, nil, fmt.Errorf("loomy: 签到应答格式无效: %w", err)
	}
	if doc.Code != "" && doc.Code != CodeOK {
		return "", before, nil, &APIError{Status: status, Code: doc.Code, Msg: pickText(doc.Message, doc.Msg, "上游业务请求失败"), Expired: isExpiredCode(doc.Code)}
	}
	after = numberField(unwrap(doc.Data), "dailyBalance")
	if after == nil {
		if again, readErr := Usage(ctx, acc); readErr == nil {
			after = again.Daily
		}
	}
	switch {
	case before == nil || after == nil:
		return "unknown", before, after, nil
	case *after > *before:
		return "claimed", before, after, nil
	default:
		return "done", before, after, nil
	}
}

// NormalizePhone 去掉空格/横线/国家码，返回 11 位手机号。
func NormalizePhone(raw string) (string, error) {
	p := strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(raw))
	if strings.HasPrefix(p, "+86") {
		p = p[3:]
	} else if len(p) == 13 && strings.HasPrefix(p, "86") {
		p = p[2:]
	}
	if !phonePattern.MatchString(p) {
		return "", errors.New("loomy: 手机号格式无效")
	}
	return p, nil
}

var (
	phonePattern     = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
	phoneCodePattern = regexp.MustCompile(`^[0-9]{6}$`)
)

// ===== 签名与请求 =====

// baseFields 是签名信封里固定不变的那一块（字段顺序与 Java 侧一致）。
type baseFields struct {
	AppID    string `json:"appid"`
	ModelID  string `json:"modelid"`
	Version  string `json:"version"`
	DeviceID string `json:"devid"`
	UA       string `json:"ua"`
	TraceID  string `json:"traceid"`
}

// signedEnvelope 是账号主机所有请求的外壳。
type signedEnvelope struct {
	Base  baseFields `json:"base"`
	Param any        `json:"param"`
}

// signedPost 打一次账号主机接口，自带完整签名头。
func signedPost(ctx context.Context, path string, param any) ([]byte, error) {
	body, err := buildSignedBody(param)
	if err != nil {
		return nil, err
	}
	headers, err := sign(path, body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AccountBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loomy: 账号请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("loomy: 读账号应答失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Msg: snippet(raw), Expired: resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden}
	}

	var doc struct {
		Code    string          `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("loomy: 账号应答格式无效: %w", err)
	}
	if doc.Code != CodeOK {
		return nil, &APIError{Status: resp.StatusCode, Code: doc.Code, Msg: pickText(doc.Message, doc.Msg, "上游业务请求失败"), Expired: isExpiredCode(doc.Code)}
	}
	return raw, nil
}

// buildSignedBody 生成 body = {"base":{...},"param":{...}}。
func buildSignedBody(param any) ([]byte, error) {
	trace, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	env := signedEnvelope{
		Base: baseFields{
			AppID:    AppID,
			ModelID:  ModelID,
			Version:  AppVersion,
			DeviceID: DeviceID,
			UA:       ClientUA,
			TraceID:  trace,
		},
		Param: param,
	}
	return json.Marshal(env)
}

// sign 计算 Content-MD5 与 Authorization 那一串签名头。
func sign(path string, body []byte) (map[string]string, error) {
	sum := md5.Sum(body)
	contentMD5 := base64.StdEncoding.EncodeToString(sum[:])

	date := time.Now().UTC().Format(dateLayout)
	nonce, err := randomUUID()
	if err != nil {
		return nil, err
	}
	canonical := "POST\n" + path + "\n\n" + contentMD5 + "\napplication/json\n" + date + "\n" + nonce + "\n\n"

	mac := hmac.New(sha1.New, []byte(SK))
	if _, err := mac.Write([]byte(canonical)); err != nil {
		return nil, err
	}
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return map[string]string{
		"Authorization": "account " + AK + ":" + signature,
		"Date":          date,
		"Nonce":         nonce,
		"Content-MD5":   contentMD5,
	}, nil
}

// gatewayRequest 打一次业务网关，带 token + Bearer 两个头。
func gatewayRequest(ctx context.Context, method, path, session string, body []byte) (int, []byte, error) {
	if strings.TrimSpace(session) == "" {
		return 0, nil, errors.New("loomy: session 无效")
	}
	for i := 0; i < len(session); i++ {
		if c := session[i]; c < 0x20 || c == 0x7f {
			return 0, nil, errors.New("loomy: session 含控制字符")
		}
	}

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, GatewayBase+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("token", session)
	req.Header.Set("Authorization", "Bearer "+session)
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("loomy: 网关请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("loomy: 读网关应答失败: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, raw, &APIError{Status: resp.StatusCode, Msg: "登录态已失效", Expired: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, raw, &APIError{Status: resp.StatusCode, Msg: snippet(raw)}
	}
	return resp.StatusCode, raw, nil
}

// ===== 应答工具 =====

// dataOf 在「账号主机」应答里取 data 对象。
func dataOf(raw []byte) map[string]any {
	var doc struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return unwrap(doc.Data)
}

// unwrap 解出 data；上游有时把真正的数据再套一层 data。
func unwrap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	if inner, ok := obj["data"].(map[string]any); ok {
		return inner
	}
	return obj
}

func stringField(obj map[string]any, key string) string {
	if obj == nil {
		return ""
	}
	if v, ok := obj[key].(string); ok {
		return v
	}
	return ""
}

func numberField(obj map[string]any, key string) *float64 {
	if obj == nil {
		return nil
	}
	switch v := obj[key].(type) {
	case float64:
		return &v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return nil
		}
		return &f
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%f", &f); err != nil {
			return nil
		}
		return &f
	}
	return nil
}

func isExpiredCode(code string) bool {
	for _, c := range ExpiredCodes {
		if c == code {
			return true
		}
	}
	return false
}

func pickText(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// randomHex 生成 2n 个小写 hex 字符（Java 侧用 md5(随机 UUID) 当 traceid）。
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// randomUUID 生成一个 RFC 4122 形状的 UUID（只用于 nonce，v4）。
func randomUUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	h := hex.EncodeToString(buf)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}
