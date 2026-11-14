package trae

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HTTPClient 是 Trae 认证与 API 请求共用的客户端。
// 注意：Transport 由 provider 包在 init 时注入（带内置 CA），否则 Android 上会 x509 失败。
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// Flow 是一次进行中的登录（对应 All-in 的 TraeControl 登录流）。
type Flow struct {
	FlowID   string
	AuthURL  string
	Deadline time.Time

	mu     sync.Mutex
	status string // pending / done / failed
	err    string
	result *Account

	server *http.Server
	ident  *Identity
}

// Account 是登录成功后的账号凭据。
type Account struct {
	UID        string `json:"uid"`
	Token      string `json:"token"`      // Cloud-IDE-JWT
	Refresh    string `json:"refresh"`    // 刷新令牌
	DeviceID   string `json:"deviceId"`
	MachineID  string `json:"machineId"`
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"`
	Expires    int64  `json:"expires,omitempty"`
	Name       string `json:"name,omitempty"`
}

// StartLogin 起一个登录流：生成本地回调服务并返回需要用户打开的授权 URL。
func StartLogin(timeout time.Duration) (*Flow, error) {
	ident, err := NewIdentity()
	if err != nil {
		return nil, err
	}
	flowID := RandomNonce()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("本地回调端口不可用: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	callback := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)

	f := &Flow{
		FlowID:   flowID,
		Deadline: time.Now().Add(timeout),
		status:   "pending",
		ident:    ident,
	}

	// 授权 URL：参数与官方桌面端一致（伪装 Windows 环境）
	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", "2.3.24254")
	q.Set("auth_type", "local")
	q.Set("client_id", "ono9krqynydwx5")
	q.Set("redirect", "0")
	q.Set("login_trace_id", flowID)
	q.Set("auth_callback_url", callback)
	q.Set("machine_id", ident.MachineID)
	q.Set("device_id", ident.DeviceID)
	q.Set("x_device_id", ident.DeviceID)
	q.Set("x_machine_id", ident.MachineID)
	q.Set("x_device_brand", Brand)
	q.Set("x_device_type", "windows")
	q.Set("x_os_version", "Windows 10 Pro")
	q.Set("x_env", "")
	q.Set("x_app_version", "3.3.65")
	q.Set("x_app_type", "stable")
	q.Set("hide_saas_login", "true")
	f.AuthURL = "https://www.trae.cn/authorization?" + q.Encode()

	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", f.handleCallback)
	f.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		_ = f.server.Serve(ln)
	}()
	// 到点自动关
	go func() {
		time.Sleep(time.Until(f.Deadline))
		f.mu.Lock()
		if f.status == "pending" {
			f.status = "expired"
			f.err = "登录超时"
		}
		f.mu.Unlock()
		if f.server != nil {
			_ = f.server.Close()
		}
	}()

	return f, nil
}

// Status 查询当前状态（供轮询接口）。
func (f *Flow) Status() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{
		"flowId": f.FlowID,
		"url":    f.AuthURL,
		"status": f.status,
	}
	if f.err != "" {
		out["error"] = f.err
	}
	return out
}

// TakeResult 取走登录结果（只能取一次）。
func (f *Flow) TakeResult() *Account {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != "done" {
		return nil
	}
	r := f.result
	f.result = nil
	return r
}

// Close 主动关闭登录流。
func (f *Flow) Close() {
	f.mu.Lock()
	if f.status == "pending" {
		f.status = "closed"
		f.err = "登录已关闭"
	}
	f.mu.Unlock()
	if f.server != nil {
		_ = f.server.Close()
	}
}

// handleCallback 处理 trae.cn 的回调。
func (f *Flow) handleCallback(w http.ResponseWriter, r *http.Request) {
	// 只接受来自 trae 域的跨域，防止任意页面伪造回调
	origin := r.Header.Get("Origin")
	if origin != "" && origin != "https://www.trae.cn" && origin != "https://trae.cn" {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	q := r.URL.Query()

	// 校验 loginTraceID，防止串流
	trace := q.Get("loginTraceID")
	if trace == "" {
		trace = q.Get("login_trace_id")
	}
	if trace != f.FlowID {
		http.Error(w, "授权状态无效", http.StatusForbidden)
		return
	}
	if q.Get("error") != "" {
		f.fail("授权未完成")
		_, _ = w.Write([]byte("授权未完成，可以关闭此页"))
		return
	}

	rawJWT := q.Get("userJwt")
	if rawJWT == "" {
		http.Error(w, "缺少授权信息", http.StatusBadRequest)
		return
	}

	// userJwt 是一个 JSON 串（不是 JWT token）
	var jwt map[string]any
	if err := json.Unmarshal([]byte(rawJWT), &jwt); err != nil {
		f.fail("授权信息解析失败")
		http.Error(w, "授权信息解析失败", http.StatusBadRequest)
		return
	}

	uid := pickString(jwt, "UID", "uid", "UserId", "userId")
	refresh := pickString(jwt, "RefreshToken", "refreshToken", "refresh")
	name := pickString(jwt, "Nickname", "nickname", "Name", "name")

	if refresh == "" {
		f.fail("授权信息里没有 RefreshToken")
		http.Error(w, "缺少 RefreshToken", http.StatusBadRequest)
		return
	}

	acc, err := ExchangeToken(f.ident, refresh)
	if err != nil {
		f.fail("换取令牌失败: " + err.Error())
		http.Error(w, "换取令牌失败", http.StatusBadGateway)
		return
	}
	acc.UID = uid
	acc.Name = name

	f.mu.Lock()
	f.status = "done"
	f.result = acc
	f.mu.Unlock()

	_, _ = w.Write([]byte("登录成功，可以关闭此页返回 All In"))
	go func() {
		time.Sleep(2 * time.Second)
		if f.server != nil {
			_ = f.server.Close()
		}
	}()
}

func (f *Flow) fail(msg string) {
	f.mu.Lock()
	f.status = "failed"
	f.err = msg
	f.mu.Unlock()
}

// ExchangeToken 用 RefreshToken 换 Cloud-IDE-JWT（会依次尝试两个 ClientID）。
func ExchangeToken(ident *Identity, refresh string) (*Account, error) {
	var lastErr error
	for _, c := range ClientCandidates {
		acc, err := exchangeOnce(ident, refresh, c.ClientID, c.Platform, c.Version)
		if err == nil {
			return acc, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func exchangeOnce(ident *Identity, refresh, clientID, platform, version string) (*Account, error) {
	at := time.Now().Unix()
	nonce := RandomNonce()
	sig, err := ident.Proof(ExchangePath, clientID, refresh, at, nonce)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"ClientID":     clientID,
		"ClientSecret": "",
		"RefreshToken": refresh,
		"DeviceInfo": map[string]any{
			"DeviceID":        ident.DeviceID,
			"MachineID":       ident.MachineID,
			"PlatformCode":    platform,
			"DeviceType":      "PC",
			"DeviceName":      "",
			"DeviceModel":     "",
			"ClientVersion":   version,
			"DevicePublicKey": ident.PublicKey,
			"DeviceBrand":     Brand,
			"DeviceCPU":       "AMD",
			"OSInfo":          "Windows",
			"OSVersion":       "10.0.22631",
		},
		"DeviceProof": map[string]any{
			"Signature": sig,
			"Timestamp": at,
			"Nonce":     nonce,
		},
		"IDEVersion": version,
	}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, AuthHost+ExchangePath, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Trae CN/"+version+" (Windows 10; AMD)")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var env struct {
		Result struct {
			Token        string `json:"Token"`
			RefreshToken string `json:"RefreshToken"`
			TokenExpireAt any   `json:"TokenExpireAt"`
		} `json:"Result"`
		responseMetadata any
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("响应解析失败（HTTP %d）", resp.StatusCode)
	}
	if env.Result.Token == "" {
		return nil, fmt.Errorf("未拿到 token（HTTP %d）", resp.StatusCode)
	}

	nextRefresh := env.Result.RefreshToken
	if nextRefresh == "" {
		nextRefresh = refresh
	}
	return &Account{
		Token:      env.Result.Token,
		Refresh:    nextRefresh,
		DeviceID:   ident.DeviceID,
		MachineID:  ident.MachineID,
		PublicKey:  ident.PublicKey,
		PrivateKey: ident.PrivateKey,
	}, nil
}

// Headers 构造调用 Trae API 需要的头（伪装 Windows 桌面端）。
func Headers(acc *Account) map[string]string {
	h := map[string]string{
		"Content-Type":        "application/json",
		"Authorization":       "Cloud-IDE-JWT " + acc.Token,
		"X-Cloudide-Token":    acc.Token,
		"x-ide-token":         acc.Token,
		"x-uid":               acc.UID,
		"x-app-id":            AppID,
		"x-device-id":         acc.DeviceID,
		"x-machine-id":        acc.MachineID,
		"x-request-id":        newUUID(),
		"x-app-version":       "0.1.69",
		"x-app-version-code":  "20260917",
		"x-ide-version":       "0.1.69",
		"x-ide-version-code":  "20260917",
		"x-ide-version-type":  "stable",
		"x-device-cpu":        "AMD",
		"x-device-brand":      Brand,
		"x-device-type":       "windows",
		"x-os-version":        "Windows 10",
		"x-system-type":       "Windows",
	}
	return h
}

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return strings.Join([]string{s[0:8], s[8:12], s[12:16], s[16:20], s[20:32]}, "-")
}
