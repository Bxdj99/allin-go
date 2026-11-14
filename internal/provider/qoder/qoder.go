// Package qoder 封装阿里「Qoder 国内版」(qoder.cn) 的设备码登录与 COSY 签名协议。
//
// 移植自 All-in 的 QoderAuth（源头是 magpie-community/plugins 的 packages/qoder，MIT）。
// 只做国内站 qoder.cn：国际站 qoder.com 在境内常被 DNS 污染，TLS 都建不起来。
//
// 认证链路（设备流，和 ZCode 一个思路）：
//
//  1. 本地生成 PKCE verifier / challenge、nonce、machineId。
//  2. 打开 https://qoder.cn/device/selectAccounts?challenge=...&challenge_method=S256
//     &nonce=...&machine_id=...&client_id=...，让用户在浏览器里登录授权。
//  3. 每 2 秒 GET https://openapi.qoder.com.cn/api/v1/deviceToken/poll
//     ?nonce=...&verifier=...&challenge_method=S256；未授权时上游回 404，属于「还在等」。
//  4. 拿到 deviceToken 后换 jobToken：POST /api/v1/me/jobToken，body {"clientId":...}。
//     国内站 CLI 从不换 jobToken，换不到就退回用 deviceToken 直接聊天。
//  5. GET /api/v1/userinfo 补 uid / 邮箱 / 昵称。
//
// 聊天**不是**普通 Bearer，而是 COSY 签名（Authorization: Bearer COSY.<payload>.<sig>）：
//   - AES-128-CBC（key 同时当 IV）加密用户信息 → base64 = info
//   - 用内置 RSA-1024 公钥 PKCS1 包裹这把 AES key → base64 = key
//   - base64(JSON{version,requestId,info,cosyVersion,ideVersion}) = payload
//   - md5(payload + "\n" + key + "\n" + ts + "\n" + bodyWire + "\n" + path) = sig
//   - 请求体本身还要过自定义 base64（打乱字母表 + 首尾各 1/3 对调），见 EncodeBody
//
// 两个容易踩的点：签名范围里的 bodyWire 是**编码之后**的串（空 body 就是空串），
// 以及 path 要去掉开头的 "/algo" 前缀。
package qoder

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// 端点与常量（全部来自 Java 侧硬编码）。
const (
	// ProviderID 是网关里这一家的标识（国内站）。
	ProviderID = "qoder_cn"

	// WebBase 是网页侧，负责设备码授权页。
	WebBase = "https://qoder.cn"
	// OpenAPIBase 是 openapi，负责设备码轮询、换 token、续期、用户信息。
	OpenAPIBase = "https://openapi.qoder.com.cn"
	// APIGateway 是模型网关，聊天与模型目录都在这儿。
	APIGateway = "https://gateway.qoder.com.cn"

	// ClientID 是国内站 CLI（@qodercn-ai/qoderclicn）的生产 client id。
	ClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

	// SelectAccountsPath 设备码授权页路径。
	SelectAccountsPath = "/device/selectAccounts"
	// DevicePollPath 设备码轮询。
	DevicePollPath = "/api/v1/deviceToken/poll"
	// JobTokenPath 用 deviceToken 换 jobToken。
	JobTokenPath = "/api/v1/me/jobToken"
	// UserInfoPath 用户信息。
	UserInfoPath = "/api/v1/userinfo"
	// JobRefreshPath jobToken 续期。
	JobRefreshPath = "/api/v1/jobToken/refresh"
	// DeviceRefreshPath deviceToken 续期。
	DeviceRefreshPath = "/api/v1/deviceToken/refresh"

	// ChatURL 聊天补全的 SSE 入口，查询参数是客户端固定带的。
	ChatURL = APIGateway + "/algo/api/v2/service/pro/sse/agent_chat_generation" +
		"?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	// ModelsURL 模型清单入口（GET，body 为空串）。
	ModelsURL = APIGateway + "/algo/api/v2/model/list?Encode=1"

	// CosyVersion 必须与桌面端实际版本一致，否则信封里的 Cosy-ClientType: 10 会被拒。
	CosyVersion = "1.1.69"
	// LoginUA 登录侧（start/poll/userinfo）用的 UA。
	LoginUA = "Magic/App"
	// QoderSystem 是上游要求的第一条 system 文本前缀（写请求体时用）。
	QoderSystem = "You are a Qoder agent. Use the instructions below and the tools available to you to assist the user."

	// PollInterval 设备码轮询间隔。
	PollInterval = 2 * time.Second
	// LoginTimeout 一次登录流程的有效期。
	LoginTimeout = 15 * time.Minute
	// Timeout 单次非流式请求的超时。
	Timeout = 20 * time.Second
)

// RSAKey 是 Qoder 桌面端内置的 1024 位公钥，用来包裹每次请求的 AES key。
const RSAKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// cosyAlphabet 是 Qoder 自定义 base64 的字母表（'$' 当填充符）。
const cosyAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

// HTTPClient 是包内共用的客户端；聊天流式请求建议调用方自建无总超时的 client。
var HTTPClient = &http.Client{Timeout: Timeout}

// Account 是一个 Qoder 国内版账号的凭据集合，字段名与配置里的 JSON 对齐。
type Account struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	// UID 是 userinfo 返回的用户 id。
	UID string `json:"uid,omitempty"`
	// Token 是聊天用的 token（jobToken，或 deviceChat 时的 deviceToken）。
	Token string `json:"token,omitempty"`
	// Refresh 是 Token 对应的续期凭据。
	Refresh string `json:"refresh,omitempty"`
	// DeviceToken 是设备流拿到的原始 token。
	DeviceToken string `json:"deviceToken,omitempty"`
	// DeviceRefresh 是 deviceToken 的续期凭据。
	DeviceRefresh string `json:"deviceRefresh,omitempty"`
	// DeviceChat 为 true 表示没换到 jobToken，直接拿 deviceToken 聊天。
	DeviceChat bool `json:"deviceChat,omitempty"`
	// ExpiresAt 是 Token 到期毫秒时间戳（0 = 未知）。
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// MachineID 是设备流的 machine_id，签 COSY 时当机器身份用。
	MachineID string `json:"machineId,omitempty"`
	// AddedAt 入池时间（毫秒）。
	AddedAt int64 `json:"addedAt,omitempty"`
}

// AccessToken 返回聊天用的 token。
func (a *Account) AccessToken() string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.Token)
}

// RefreshTokenValue 返回该用哪一条续期凭据。
func (a *Account) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	if a.DeviceChat && strings.TrimSpace(a.Refresh) == "" {
		return strings.TrimSpace(a.DeviceRefresh)
	}
	return strings.TrimSpace(a.Refresh)
}

// APIError 是上游的失败应答（状态码不是 2xx）。
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	if e == nil {
		return "qoder: <nil>"
	}
	return fmt.Sprintf("qoder: HTTP %d: %s", e.Status, e.Msg)
}

// Headers 返回「无 body」请求（模型目录、账户类接口）的 COSY 头。
//
// 签名覆盖的是空 body，所以只能用在 GET 或空 body 的端点上；带 body 的请求
// 必须用 ChatHeaders，否则签名对不上会被 401/403。
//
// 签名在数学上不会失败（密钥固定、key 固定 16 字节），失败时这里只会返回不含
// Authorization 的降级头，调用方若需要严格报错请用 ChatHeaders。
func Headers(acc *Account) map[string]string {
	h, err := cosyHeaders(ModelsURL, acc, "")
	if err != nil {
		return map[string]string{
			"Accept":       "application/json",
			"Content-Type": "application/json",
		}
	}
	return h
}

// ChatHeaders 返回聊天请求的完整头集合。
//
// bodyWire 必须是 EncodeBody 之后的线上串；modelKey / modelSource 取自请求体的
// model_config.key / model_config.source，没有就传空串（那两个头按需下发）。
func ChatHeaders(acc *Account, bodyWire, modelKey, modelSource string) (map[string]string, error) {
	if acc == nil || strings.TrimSpace(acc.MachineID) == "" {
		return nil, errors.New("qoder: 账号缺少 machine ID，请重新登录")
	}
	h, err := cosyHeaders(ChatURL, acc, bodyWire)
	if err != nil {
		return nil, err
	}
	// 聊天与普通接口的头集合不同：这三个必须去掉，否则上游直接 500。
	// （MachineToken/MachineType 本该是桌面端 runtime-info 的真实设备身份，
	// 手机/服务端拿不到，就省略，而不是拿 machineId 顶替。）
	delete(h, "Cosy-ClientIp")
	delete(h, "Cosy-MachineToken")
	delete(h, "Cosy-MachineType")
	if host := machineHostname(); host != "" {
		h["Cosy-MachineHostname"] = host
	}
	h["Accept"] = "text/event-stream"
	h["Cache-Control"] = "no-cache"
	if modelKey != "" {
		h["X-Model-Key"] = modelKey
	}
	if modelSource != "" {
		h["X-Model-Source"] = modelSource
	}
	return h, nil
}

// cosyHeaders 生成一整套 COSY 签名头；bodyWire 是编码后的请求体（无 body 传空串）。
func cosyHeaders(rawURL string, acc *Account, bodyWire string) (map[string]string, error) {
	if acc == nil {
		return nil, errors.New("qoder: 账号为空")
	}
	ts := time.Now().Unix()

	info, wrappedKey, err := userBlob(acc)
	if err != nil {
		return nil, err
	}
	requestID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	wrapper := struct {
		Version     string `json:"version"`
		RequestID   string `json:"requestId"`
		Info        string `json:"info"`
		CosyVersion string `json:"cosyVersion"`
		IdeVersion  string `json:"ideVersion"`
	}{
		Version:     "v1",
		RequestID:   requestID,
		Info:        info,
		CosyVersion: CosyVersion,
		IdeVersion:  "",
	}
	wrapperJSON, err := json.Marshal(wrapper)
	if err != nil {
		return nil, err
	}
	payload := base64.StdEncoding.EncodeToString(wrapperJSON)

	path, err := signPath(rawURL)
	if err != nil {
		return nil, err
	}
	sig := md5Hex(fmt.Sprintf("%s\n%s\n%d\n%s\n%s", payload, wrappedKey, ts, bodyWire, path))

	machine := strings.TrimSpace(acc.MachineID)
	if machine == "" {
		machine, err = randomHex(32)
		if err != nil {
			return nil, err
		}
	}

	return map[string]string{
		"Accept":                "application/json",
		"Accept-Encoding":       "identity",
		"Content-Type":          "application/json",
		"Authorization":         "Bearer COSY." + payload + "." + sig,
		"Cosy-Business-Product": "app",
		"Cosy-Business-Type":    "agent",
		"Cosy-ClientIp":         machine,
		"Cosy-ClientType":       "10",
		"Cosy-Data-Policy":      "disagree",
		"Cosy-Date":             fmt.Sprintf("%d", ts),
		"Cosy-Key":              wrappedKey,
		"Cosy-MachineId":        machine,
		"Cosy-MachineToken":     machine,
		"Cosy-MachineType":      "5",
		"Cosy-MachineOS":        machineOS(),
		"Cosy-Scene":            "app",
		"Cosy-User":             acc.UID,
		"Cosy-Version":          CosyVersion,
		"Login-Version":         "v2",
	}, nil
}

// userBlob 把用户信息加密成 COSY 信封的 info / key 两段。
func userBlob(acc *Account) (info string, wrappedKey string, err error) {
	plain, err := json.Marshal(struct {
		UID   string `json:"uid"`
		AID   string `json:"aid"`
		Name  string `json:"name"`
		Email string `json:"email"`
		// 上游要的是 security_oauth_token 字段，装的就是聊天 token 本身。
		SecurityOAuthToken string `json:"security_oauth_token"`
	}{
		UID:                acc.UID,
		AID:                "",
		Name:               acc.Name,
		Email:              acc.Email,
		SecurityOAuthToken: acc.AccessToken(),
	})
	if err != nil {
		return "", "", err
	}

	// AES-128-CBC，key 同时当 IV，key 取 16 个 ASCII 字符（每个请求随机一把）。
	keyStr, err := randomHex(8)
	if err != nil {
		return "", "", err
	}
	keyBytes := []byte(keyStr)
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return "", "", err
	}
	padded := pkcs5Pad(plain, block.BlockSize())
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, keyBytes).CryptBlocks(encrypted, padded)
	info = base64.StdEncoding.EncodeToString(encrypted)

	pub, err := rsaPublicKey()
	if err != nil {
		return "", "", err
	}
	sealed, err := rsa.EncryptPKCS1v15(rand.Reader, pub, keyBytes)
	if err != nil {
		return "", "", err
	}
	return info, base64.StdEncoding.EncodeToString(sealed), nil
}

func rsaPublicKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(RSAKey))
	if block == nil {
		return nil, errors.New("qoder: RSA 公钥解析失败")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("qoder: RSA 公钥解析失败: %w", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("qoder: 内置公钥不是 RSA")
	}
	return pub, nil
}

// signPath 取签名用的 path：只要 path 部分，并去掉开头的 "/algo" 前缀。
func signPath(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("qoder: URL 解析失败: %w", err)
	}
	path := u.Path
	if strings.HasPrefix(path, "/algo") {
		path = path[len("/algo"):]
	}
	return path, nil
}

// machineOS 返回机器的目标描述，形如 aarch64_linux。
func machineOS() string {
	arch := runtime.GOARCH
	switch arch {
	case "arm64", "aarch64":
		arch = "aarch64"
	case "amd64", "x86_64":
		arch = "x86_64"
	}
	return arch + "_linux"
}

// machineHostname 返回可打印 ASCII 的机器名；拿不到就返回空串（宁缺勿造）。
func machineHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	safe := true
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x21 || c > 0x7e {
			safe = false
			break
		}
	}
	if safe {
		if len(name) > 96 {
			return name[:88]
		}
		return name
	}
	out := make([]byte, 0, len(name))
	prevDash := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 0x21 && c <= 0x7e {
			out = append(out, c)
			prevDash = false
			continue
		}
		if !prevDash {
			out = append(out, '-')
			prevDash = true
		}
	}
	v := strings.Trim(string(out), "-")
	if v == "" {
		return "unknown"
	}
	if len(v) > 96 {
		v = v[:88]
	}
	return v
}

// ===== 请求体编码（自定义 base64 + 首尾 1/3 对调）=====

// EncodeBody 把 JSON 文本编码成线上字节串。
func EncodeBody(text string) string {
	return swapThirds(segmentEncode([]byte(text)))
}

// DecodeBody 把线上字节串解回 JSON 文本。
func DecodeBody(wire string) string {
	s := swapThirds(wire)
	var out bytes.Buffer
	out.Grow(len(s) * 3 / 4)
	for g := 0; g+4 <= len(s); g += 4 {
		vals := make([]int, 0, 4)
		for k := 0; k < 4; k++ {
			c := s[g+k]
			if c == '$' {
				continue
			}
			idx := strings.IndexByte(cosyAlphabet, c)
			if idx < 0 {
				continue
			}
			vals = append(vals, idx)
		}
		if len(vals) == 0 {
			continue
		}
		var x uint64
		for _, v := range vals {
			x = x*64 + uint64(v)
		}
		n := 6 * len(vals) / 8
		for k := 0; k < n; k++ {
			shift := 6*len(vals) - 8*(k+1)
			if shift < 0 {
				break
			}
			out.WriteByte(byte((x >> uint(shift)) & 0xff))
		}
	}
	return out.String()
}

// segmentEncode 是自定义 base64：用 cosyAlphabet，'$' 当填充。
func segmentEncode(data []byte) string {
	var b strings.Builder
	b.Grow(len(data)*4/3 + 4)
	acc, nb := 0, 0
	for _, raw := range data {
		acc = ((acc << 8) | int(raw)) & 0xffffff
		nb += 8
		for nb >= 6 {
			nb -= 6
			b.WriteByte(cosyAlphabet[(acc>>uint(nb))&63])
		}
	}
	if nb > 0 {
		b.WriteByte(cosyAlphabet[(acc<<uint(6-nb))&63])
	}
	for b.Len()%4 != 0 {
		b.WriteByte('$')
	}
	return b.String()
}

// swapThirds 把首尾各 1/3 对调。
func swapThirds(s string) string {
	t := len(s) / 3
	if t == 0 {
		return s
	}
	return s[len(s)-t:] + s[t:len(s)-t] + s[:t]
}

// ===== 设备流登录 =====

// Flow 是一次进行中的设备码登录。
type Flow struct {
	Verifier     string
	Nonce        string
	MachineID    string
	AuthorizeURL string
	Deadline     time.Time
	Interval     time.Duration
}

// PollResult 是一次轮询的结果；Status 取 pending / ready。
type PollResult struct {
	Status  string
	Error   string
	Account *Account
}

// StartDeviceFlow 开一个登录流程，返回要展示给用户授权的页面地址。
func StartDeviceFlow() (*Flow, error) {
	verifierBytes := make([]byte, 64)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	nonce, err := randomUUID()
	if err != nil {
		return nil, err
	}
	machineID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	challenge := sha256Base64URL(verifier)

	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("machine_id", machineID)
	q.Set("client_id", ClientID)

	return &Flow{
		Verifier:     verifier,
		Nonce:        nonce,
		MachineID:    machineID,
		AuthorizeURL: WebBase + SelectAccountsPath + "?" + q.Encode(),
		Deadline:     time.Now().Add(LoginTimeout),
		Interval:     PollInterval,
	}, nil
}

// PollDeviceFlow 轮询一次；拿到底层 token 后自动换 jobToken 并读用户信息。
func PollDeviceFlow(ctx context.Context, f *Flow) (*PollResult, error) {
	if f == nil {
		return nil, errors.New("qoder: 登录流程为空")
	}
	q := url.Values{}
	q.Set("nonce", f.Nonce)
	q.Set("verifier", f.Verifier)
	q.Set("challenge_method", "S256")
	status, raw, err := loginRequest(ctx, http.MethodGet, OpenAPIBase+DevicePollPath+"?"+q.Encode(), "", nil)
	if err != nil {
		return &PollResult{Status: "pending", Error: err.Error()}, nil
	}
	// 未授权时上游回 404，这不是错误，只是「还在等」。
	if status < 200 || status >= 300 {
		return &PollResult{Status: "pending", Error: fmt.Sprintf("HTTP %d", status)}, nil
	}

	var doc struct {
		Token        string          `json:"token"`
		DeviceToken  string          `json:"device_token"`
		RefreshToken string          `json:"refresh_token"`
		UserID       json.RawMessage `json:"user_id"`
		UserName     string          `json:"user_name"`
		ExpireTime   json.RawMessage `json:"expire_time"`
		ExpiresAt    json.RawMessage `json:"expires_at"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Data         json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return &PollResult{Status: "pending", Error: "应答格式无效"}, nil
	}
	deviceToken := firstNonEmpty(doc.Token, doc.DeviceToken)
	if deviceToken == "" && len(doc.Data) > 0 {
		var inner struct {
			Token        string          `json:"token"`
			DeviceToken  string          `json:"device_token"`
			RefreshToken string          `json:"refresh_token"`
			UserID       json.RawMessage `json:"user_id"`
			UserName     string          `json:"user_name"`
			ExpireTime   json.RawMessage `json:"expire_time"`
			ExpiresAt    json.RawMessage `json:"expires_at"`
			ExpiresIn    json.RawMessage `json:"expires_in"`
		}
		if json.Unmarshal(doc.Data, &inner) == nil {
			deviceToken = firstNonEmpty(inner.Token, inner.DeviceToken)
			doc.RefreshToken = firstNonEmpty(doc.RefreshToken, inner.RefreshToken)
			doc.UserName = firstNonEmpty(doc.UserName, inner.UserName)
			if len(doc.UserID) == 0 {
				doc.UserID = inner.UserID
			}
			if len(doc.ExpireTime) == 0 {
				doc.ExpireTime = inner.ExpireTime
			}
			if len(doc.ExpiresAt) == 0 {
				doc.ExpiresAt = inner.ExpiresAt
			}
			if len(doc.ExpiresIn) == 0 {
				doc.ExpiresIn = inner.ExpiresIn
			}
		}
	}
	if deviceToken == "" {
		return &PollResult{Status: "pending"}, nil
	}

	acc := &Account{
		DeviceToken:   deviceToken,
		DeviceRefresh: doc.RefreshToken,
		MachineID:     f.MachineID,
		Name:          strings.TrimSpace(doc.UserName),
		UID:           rawToText(doc.UserID),
	}

	// 换 jobToken；国内站常见路径是换不到，那就退回 deviceToken 直接聊天。
	gotJob := false
	jobBody, _ := json.Marshal(map[string]string{"clientId": ClientID})
	if jStatus, jRaw, jErr := loginRequest(ctx, http.MethodPost, OpenAPIBase+JobTokenPath, deviceToken, jobBody); jErr == nil && jStatus >= 200 && jStatus < 300 {
		var jo struct {
			Token        string          `json:"token"`
			DeviceToken  string          `json:"device_token"`
			AccessToken  string          `json:"access_token"`
			RefreshToken string          `json:"refresh_token"`
			ExpireTime   json.RawMessage `json:"expire_time"`
			ExpiresAt    json.RawMessage `json:"expires_at"`
			ExpiresIn    json.RawMessage `json:"expires_in"`
		}
		if json.Unmarshal(jRaw, &jo) == nil {
			if jt := firstNonEmpty(jo.Token, jo.DeviceToken, jo.AccessToken); jt != "" {
				acc.Token = jt
				acc.Refresh = jo.RefreshToken
				acc.ExpiresAt = expiresFrom(jo.ExpireTime, jo.ExpiresAt, jo.ExpiresIn, false)
				gotJob = true
			}
		}
	}
	if !gotJob {
		acc.Token = deviceToken
		acc.Refresh = acc.DeviceRefresh
		acc.DeviceChat = true
		acc.ExpiresAt = expiresFrom(doc.ExpireTime, doc.ExpiresAt, doc.ExpiresIn, false)
	}

	// 读用户信息补 uid / 邮箱 / 昵称。
	if uStatus, uRaw, uErr := loginRequest(ctx, http.MethodGet, OpenAPIBase+UserInfoPath, deviceToken, nil); uErr == nil && uStatus >= 200 && uStatus < 300 {
		var uo struct {
			ID     json.RawMessage `json:"id"`
			UserID json.RawMessage `json:"user_id"`
			UID    json.RawMessage `json:"uid"`
			Email  string          `json:"email"`
			Name   string          `json:"name"`
		}
		if json.Unmarshal(uRaw, &uo) == nil {
			if id := firstNonEmpty(rawToText(uo.ID), rawToText(uo.UserID), rawToText(uo.UID)); id != "" {
				acc.UID = id
			}
			if e := strings.TrimSpace(uo.Email); e != "" {
				acc.Email = e
			}
			if n := strings.TrimSpace(uo.Name); n != "" {
				acc.Name = n
			}
		}
	}

	acc.AddedAt = time.Now().UnixMilli()
	acc.ID = firstNonEmpty(acc.Email, acc.UID)
	return &PollResult{Status: "ready", Account: acc}, nil
}

// ===== 续期 =====

// RefreshToken 用 refresh_token 换一对新 token，成功后原地更新 acc。
//
// 端点按 DeviceChat 分叉：deviceChat 走 /api/v1/deviceToken/refresh，否则走
// /api/v1/jobToken/refresh；body 只有 {"refresh_token": ...}。
// **这个 refresh token 是一次性的**：失败不代表可以重放。
func RefreshToken(ctx context.Context, acc *Account) error {
	if acc == nil {
		return errors.New("qoder: 账号为空")
	}
	refresh := acc.RefreshTokenValue()
	if refresh == "" {
		return errors.New("qoder: 缺少 refresh token")
	}
	path := JobRefreshPath
	if acc.DeviceChat {
		path = DeviceRefreshPath
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": refresh})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, OpenAPIBase+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// 故意不发 User-Agent：上游这个端点本来就没有声明过 UA，
	// 凭空编一个指纹等于给自己加一条未经验证的假设。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("qoder: 续期请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("qoder: 读续期应答失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, Msg: snippet(raw)}
	}

	var doc struct {
		Token        string          `json:"token"`
		DeviceToken  string          `json:"device_token"`
		RefreshToken string          `json:"refresh_token"`
		ExpireTime   json.RawMessage `json:"expire_time"`
		ExpiresAt    json.RawMessage `json:"expires_at"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 解析器的报文可能夹带刚签发的 token，所以只报类别、不回显原文。
		return errors.New("qoder: 续期应答不是 token 对")
	}
	token := firstNonEmpty(doc.Token, doc.DeviceToken)
	if token == "" || strings.TrimSpace(doc.RefreshToken) == "" {
		return errors.New("qoder: 续期应答的 token 对不完整")
	}

	acc.Token = token
	acc.Refresh = strings.TrimSpace(doc.RefreshToken)
	if acc.DeviceChat {
		acc.DeviceToken = token
		acc.DeviceRefresh = acc.Refresh
	}
	acc.ExpiresAt = expiresFrom(doc.ExpireTime, doc.ExpiresAt, doc.ExpiresIn, acc.DeviceChat)
	return nil
}

// ===== 模型目录 =====

// Models 拉官方模型清单（GET /algo/api/v2/model/list?Encode=1，body 为空）。
//
// 应答可能是编码过的（不以 '{' 开头），要先用 DecodeBody 解。
// 结构是 {"app":[{key,enable,...}],"chat":[...]}：先按 app 的顺序，再把 chat 里
// 多出来的项按相对位置插进去，跳过 "auto"/"default"，只留 enable=true。
func Models(ctx context.Context, acc *Account) ([]string, error) {
	if acc == nil || acc.MachineID == "" {
		return nil, errors.New("qoder: 账号缺少 machine ID，请重新登录")
	}
	headers, err := cosyHeaders(ModelsURL, acc, "")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qoder: 拉模型目录失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("qoder: 读模型目录失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Msg: snippet(raw)}
	}

	text := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(text, "{") {
		text = DecodeBody(text)
	}

	var root struct {
		App  []modelEntry `json:"app"`
		Chat []modelEntry `json:"chat"`
	}
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return nil, fmt.Errorf("qoder: 模型目录格式无效: %w", err)
	}
	if root.App == nil && root.Chat == nil {
		return nil, errors.New("qoder: 模型目录缺少 app/chat 段")
	}

	// 先按 app 段的顺序铺底，再把 chat 段里多出来的项按相对位置插进去
	// （对齐 Java 侧 readCatalog 的合并规则，顺序会影响面板上的模型排序）。
	order := make([]string, 0, len(root.App)+len(root.Chat))
	byKey := make(map[string]modelEntry, len(root.App)+len(root.Chat))
	for _, m := range root.App {
		key := strings.TrimSpace(m.Key)
		if key == "" || !m.Enabled.Valid() {
			continue
		}
		if _, seen := byKey[key]; seen {
			continue
		}
		byKey[key] = m
		order = append(order, key)
	}
	after := -1
	for _, m := range root.Chat {
		key := strings.TrimSpace(m.Key)
		if key == "" || !m.Enabled.Valid() {
			continue
		}
		if _, seen := byKey[key]; seen {
			for i, k := range order {
				if k == key {
					after = i
					break
				}
			}
			continue
		}
		byKey[key] = m
		after++
		if after >= len(order) {
			order = append(order, key)
			after = len(order) - 1
		} else {
			order = append(order, "")
			copy(order[after+1:], order[after:])
			order[after] = key
		}
	}

	models := make([]string, 0, len(order))
	for _, key := range order {
		if key == "" || key == "auto" || key == "default" {
			continue
		}
		if !byKey[key].Enabled.Enabled() {
			continue
		}
		models = append(models, key)
	}
	if len(models) == 0 {
		return nil, errors.New("qoder: 模型目录为空")
	}
	return models, nil
}

// modelEntry 是目录里一行。
type modelEntry struct {
	Key         string       `json:"key"`
	DisplayName string       `json:"display_name"`
	Enabled     enabledField `json:"enable"`
	MaxInput    int          `json:"max_input_tokens"`
}

// enabledField 用来区分「字段缺失」和「字段是 false」：上游要求 enable 必须是布尔。
// 与 Java 侧唯一的差别在这里——那边遇到非布尔 enable 会让整份目录读取失败，
// 这里选择跳过这一条（一份目录里个别脏行不该把整家模型都变成不可用）。
type enabledField struct {
	present bool
	value   bool
}

// Valid 判断这个字段到底有没有出现过。
func (f enabledField) Valid() bool { return f.present }

// Enabled 返回布尔值本身。
func (f enabledField) Enabled() bool { return f.value }

// UnmarshalJSON 记录「出现过」这件事。
func (f *enabledField) UnmarshalJSON(data []byte) error {
	f.present = true
	var v bool
	if err := json.Unmarshal(data, &v); err != nil {
		f.present = false
		return nil
	}
	f.value = v
	return nil
}

// ===== 内部工具 =====

// loginRequest 打一次登录侧接口（openapi），按需带 Bearer。
func loginRequest(ctx context.Context, method, rawURL, token string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", LoginUA)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// expiresFrom 解析到期时间：expire_time / expires_at / expires_in 都试一遍，
// 都没有就按一天算（device 站点优先认 expires_at）。
func expiresFrom(expireTime, expiresAt, expiresIn json.RawMessage, device bool) int64 {
	now := time.Now().UnixMilli()
	if device {
		if v := epochMillis(expiresAt); v > 0 {
			return v
		}
	} else {
		for _, raw := range []json.RawMessage{expireTime, expiresAt} {
			if v := epochMillis(raw); v > now {
				return v
			}
		}
	}
	if v, ok := rawFloat(expiresIn); ok && v > 0 {
		if device {
			v *= 1000
		}
		if v < float64((1<<62)-now) {
			return now + int64(v)
		}
	}
	return now + 24*60*60*1000
}

// epochMillis 把秒或毫秒的时间戳统一成毫秒，取不到返回 0。
func epochMillis(raw json.RawMessage) int64 {
	v, ok := rawFloat(raw)
	if !ok || v <= 0 {
		return 0
	}
	iv := int64(v)
	if iv > 1000000000000 { // 已经是毫秒
		return iv
	}
	return iv * 1000
}

func rawFloat(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return 0, false
		}
		n = json.Number(trimmed)
	}
	f, err := n.Float64()
	if err != nil {
		return 0, false
	}
	return f, true
}

// rawToText 把可能是数字也可能是字符串的字段统一成字符串。
func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

func pkcs5Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

func md5Hex(v string) string {
	sum := md5.Sum([]byte(v))
	return hex.EncodeToString(sum[:])
}

func sha256Base64URL(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// randomUUID 生成 RFC 4122 形状的 UUID（v4）。
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

func firstNonEmpty(values ...string) string {
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
