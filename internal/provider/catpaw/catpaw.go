// Package catpaw 适配美团「妙手 CatPaw」（ai.catpaw.meituan.com）的私有协议。
//
// 协议全部抄自 All-in 的 CatPawAuth.java / CatPawToolBridge.java，上游溯源是
// magpie-community/plugins 的 packages/catpaw（MIT）。按妙手桌面客户端抓包还原：
//
//	登录：GET  https://catx.nocode.cn/api/gateway/passport/login-config  拿官方登录入口
//	      （浏览器登录占用 sid 之后）GET /api/gateway/passport/poll-token?sid=  取 token
//	      GET  /api/gateway/passport/current-user                       补 uid / 昵称
//	聊天：POST https://ai.catpaw.meituan.com/api/agent/conversation/round   建会话
//	      POST .../api/agent/conversation/event                            标记 running
//	      POST .../api/agent/conversation/turn                             SSE 答案
//
// 三个必须照抄、不能「优化」的点（Java 侧注释里也是这么写的）：
//
//  1. 请求体的 source/mode 只能是 CatX / CATX_APP。抓包里的 CatPawCLI / CLI 已被网关
//     停用（返回 code=400 "source 不存在或未启用: CatPawCLI"）。
//  2. 余额与账号信息在另一个域（catx.nocode.cn）且只认 X-Auth-Token；把聊天用的
//     Cookie: X-Passport-Token 发过去会稳定 401 code 4010。
//  3. 会话协议只收一段扁平文本 prompt：工具定义、工具结果、图片都送不进去，
//     带这些的请求必须明确拒绝，不能悄悄丢掉。
//
// 另外：这个账号体系没有 refresh 接口 —— passport token 过期只能重新走一遍网页登录。
// 因此本包不提供 RefreshToken，只提供 Validate 用来判断 token 是否还活着。
package catpaw

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 协议常量。全部对应 CatPawAuth.java 里硬编码的那一套。
const (
	// ProviderID 是供应商 id（config 里的 "catpaw"）。
	ProviderID = "catpaw"

	// Origin 聊天与模型目录域（美团）。
	Origin = "https://ai.catpaw.meituan.com"
	// Passport 登录 / 账号 / 余额域（catx.nocode.cn）。
	Passport = "https://catx.nocode.cn"
	// BalanceURL 额度查询（注意是 catx.nocode.cn，不是 Origin）。
	BalanceURL = Passport + "/api/gateway/credit/balance"

	// PathModelTypes 模型目录（POST，返回 modelTypeId / modelTypeName / supportImage）。
	PathModelTypes = "/api/agent/maas/model-types"
	// PathRound 建会话（三步会话机的第一步）。
	PathRound = "/api/agent/conversation/round"
	// PathEvent 标记会话状态。
	PathEvent = "/api/agent/conversation/event"
	// PathTurn 真正的回答（SSE，累积帧）。
	PathTurn = "/api/agent/conversation/turn"

	// PathLoginConfig 取官方登录入口。
	PathLoginConfig = "/api/gateway/passport/login-config"
	// PathPollToken 用 sid 轮询登录结果。
	PathPollToken = "/api/gateway/passport/poll-token"
	// PathCurrentUser 取 uid / 昵称。
	PathCurrentUser = "/api/gateway/passport/current-user"

	// AppKey 客户端 appkey（m-appkey 头）。
	AppKey = "com.sankuai.mcopilot.cli"
	// AgentVersion 客户端版本（x-agent-version 头）。
	AgentVersion = "1.2.0-beta.6"
	// ClientUserAgent 客户端 UA（axios/1.19.0，不是 curl）。
	ClientUserAgent = "axios/1.19.0"

	// Source 请求体 source 字段，只能是 CatX。
	Source = "CatX"
	// Mode 请求体 mode 字段，只能是 CATX_APP。
	Mode = "CATX_APP"
	// PermissionMode 客户端固定发的权限模式。
	PermissionMode = "unsafeBypassPermissions"
	// ToolVersion 客户端固定发的工具版本。
	ToolVersion = "2.0.2"

	// Tenant 模型目录的 tenant。
	Tenant = "CatDesk"
	// Scene 模型目录的 scene。
	Scene = "CATX_APP"
	// Env 模型目录的 env。
	Env = "EXTERNAL"

	// HeaderBalanceGraySet 余额接口的灰度集头。
	HeaderBalanceGraySet = "new-agent-sdk"

	// LoginRedirect 登录回跳地址。实际不监听（服务端轮询 sid），
	// 只是官方登录页要求带上一个合法回调。
	LoginRedirect = "http://127.0.0.1:9/catpaw-callback"
)

// HTTPClient 是默认客户端。turn 是长连接流式（最长 15 分钟），超时交给 ctx。
var HTTPClient = &http.Client{}

// 业务错误码：登录态过期。对应 Java 侧 balance() 里 4010 / 4011 的处理。
const (
	// CodeSignInExpired 登录态过期。
	CodeSignInExpired = 4010
	// CodeSignInExpired2 登录态过期的另一种返回。
	CodeSignInExpired2 = 4011
)

// ErrSignInRequired 表示登录态失效，只能重新走网页登录。
var ErrSignInRequired = errors.New("catpaw: 登录态已过期，需要重新登录")

// ErrUnsupported 表示请求内容超出这段扁平文本协议的表达能力（工具 / 图片等）。
var ErrUnsupported = errors.New("catpaw: 请求内容不受支持")

// Model 是模型目录的一项。Type 就是上游的 modelTypeId。
type Model struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  int    `json:"modelType"`
	Image bool   `json:"image"`
}

// Models 是桌面客户端自带的模型表（id → 展示名 / modelType / 是否收图），
// 与 CatPawAuth.MODELS 一致。目录接口拿不到时当兜底，也在目录校验时用来把
// modelTypeId 反查成 id。
var Models = []Model{
	{ID: "longcat-2.0", Name: "LongCat-2.0", Type: 77, Image: true},
	{ID: "glm-5.3-flashx", Name: "GLM-5.3-FlashX", Type: 98, Image: true},
	{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", Type: 91, Image: true},
	{ID: "glm-5.3", Name: "GLM-5.3", Type: 89, Image: false},
	{ID: "deepseek-v4-flash", Name: "DeepSeek-V4-Flash", Type: 63, Image: false},
	{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro", Type: 64, Image: false},
	{ID: "minimax-m3", Name: "MiniMax-M3", Type: 70, Image: true},
	{ID: "kimi-k3", Name: "Kimi-K3", Type: 83, Image: true},
	{ID: "auto", Name: "Auto (自动选择)", Type: 0, Image: false},
}

// ModelTypeOf 返回 model 对应的 modelTypeId；未知模型回落到 77（与插件一致）。
func ModelTypeOf(id string) int {
	for _, m := range Models {
		if m.ID == id {
			return m.Type
		}
	}
	return 77
}

// Account 是一个妙手账号。
//
// Token 就是浏览器 Cookie 里 X-Passport-Token 的值；这个体系没有 refresh_token，
// 过期只能重新登录（GatewayServer.addCatPawAccount 的注释也是这么说的）。
type Account struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	UID     string `json:"uid,omitempty"`
	Token   string `json:"token,omitempty"`
	AddedAt int64  `json:"addedAt,omitempty"`
	Enabled bool   `json:"enabled,omitempty"`
}

// Headers 返回聊天 / 目录接口需要的头。
//
// m-host（客户端本机局域网 IP）在 Java 侧是明确不发的：伪造不了，发了反而更可疑。
// trace 头每次请求都要新的，所以这里现场生成。
func Headers(acc *Account) map[string]string {
	token := ""
	if acc != nil {
		token = acc.Token
	}
	trace := randomHex(16)
	return map[string]string{
		"Content-Type":    "application/json",
		"Cookie":          "X-Passport-Token=" + token,
		"User-Agent":      ClientUserAgent,
		"m-appkey":        AppKey,
		"m-debug":         "true",
		"m-traceid":       trace,
		"x-request-id":    uuidLike(),
		"x-trace-id":      trace,
		"x-agent-version": AgentVersion,
	}
}

// spanHeaders 在 Headers 之上加链路 span 头（round / event / turn 各不相同）。
func spanHeaders(acc *Account, spanID, spanName string) map[string]string {
	h := Headers(acc)
	h["m-spanid"] = spanID
	h["m-spanname"] = spanName
	return h
}

// ===================== 登录（官方网页通道，服务端轮询，无需回跳端口） =====================

// Flow 是一次进行中的登录。
//
// 妙手客户端有两条登录通道，这里只用 poll-token 那条：浏览器登录占用 sid 之后，
// 服务端 GET poll-token?sid= 直接拿 token —— 服务端对服务端，不监听本机端口，
// 也不受浏览器私有网络检查（PNA）影响。
type Flow struct {
	SID      string
	State    string
	EntryURL string
	Deadline time.Time
}

// StartLogin 向 login-config 取官方登录入口，并挂上 state / sid / redirect。
func StartLogin(ctx context.Context) (*Flow, error) {
	var cfg struct {
		Data struct {
			LoginEntryURL string `json:"loginEntryUrl"`
		} `json:"data"`
	}
	if err := getJSON(ctx, Passport+PathLoginConfig, nil, &cfg); err != nil {
		return nil, fmt.Errorf("catpaw: 无法连接登录服务: %w", err)
	}
	entry := strings.TrimSpace(cfg.Data.LoginEntryURL)
	if entry == "" {
		return nil, errors.New("catpaw: 登录服务未返回登录入口地址")
	}
	state := randomHex(16)
	sid := randomHex(16)
	sep := "?"
	if strings.Contains(entry, "?") {
		sep = "&"
	}
	entry = entry + sep +
		"state=" + state +
		"&sid=" + sid +
		"&redirect=" + url.QueryEscape(LoginRedirect)

	return &Flow{
		SID:      sid,
		State:    state,
		EntryURL: entry,
		Deadline: time.Now().Add(10 * time.Minute),
	}, nil
}

// PollLogin 轮询 sid 是否已被浏览器登录占用。
//
// 还没登录时返回 (nil, nil)，调用方按自己的节奏重试；拿到 token 后会顺手补 uid / 昵称
// （补失败不影响登录，token 已经在手）。
func PollLogin(ctx context.Context, flow *Flow) (*Account, error) {
	if flow == nil || flow.SID == "" {
		return nil, errors.New("catpaw: 登录流没有 sid")
	}
	u := Passport + PathPollToken + "?sid=" + url.QueryEscape(flow.SID)
	body, err := getRaw(ctx, u, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("catpaw: poll-token 响应解析失败: %w", err)
	}
	var token string
	if json.Unmarshal(resp.Data, &token) != nil || strings.TrimSpace(token) == "" {
		return nil, nil // 还没登录
	}
	token = strings.TrimSpace(token)

	acc := &Account{Token: token, Name: "妙手网页登录", AddedAt: time.Now().UnixMilli(), Enabled: true}
	if uid, name, err := CurrentUser(ctx, token); err == nil {
		acc.UID = uid
		if name != "" {
			acc.Name = name
		}
	}
	return acc, nil
}

// CurrentUser 用 X-Auth-Token 查 uid / 昵称（注意不是聊天那套 Cookie）。
func CurrentUser(ctx context.Context, token string) (uid, name string, err error) {
	h := map[string]string{"Accept": "application/json", "X-Auth-Token": token}
	var j struct {
		Data struct {
			UserID   json.RawMessage `json:"userId"`
			UserName string          `json:"userName"`
		} `json:"data"`
	}
	if err = getJSON(ctx, Passport+PathCurrentUser, h, &j); err != nil {
		return "", "", err
	}
	id := strings.Trim(string(j.Data.UserID), `"`)
	if id != "null" && id != "" {
		uid = id
	}
	return uid, j.Data.UserName, nil
}

// Validate 判断登录态是否还活着（余额接口是最便宜的探针）。
func Validate(ctx context.Context, acc *Account) error {
	_, _, err := Balance(ctx, acc)
	return err
}

// Balance 查额度。返回余额文本与套餐名；登录态过期时返回 ErrSignInRequired。
func Balance(ctx context.Context, acc *Account) (credit, plan string, err error) {
	if acc == nil || acc.Token == "" {
		return "", "", ErrSignInRequired
	}
	h := map[string]string{
		"Accept":       "application/json",
		"X-Auth-Token": acc.Token,
		"gray-set":     HeaderBalanceGraySet,
	}
	var j struct {
		Code int                        `json:"code"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err = getJSON(ctx, BalanceURL, h, &j); err != nil {
		return "", "", err
	}
	if j.Code == CodeSignInExpired || j.Code == CodeSignInExpired2 {
		return "", "", ErrSignInRequired
	}
	if j.Code != 0 {
		return "", "", fmt.Errorf("catpaw: 余额查询返回异常 code=%d", j.Code)
	}
	if j.Data == nil {
		return "", "", errors.New("catpaw: 余额响应缺少 data")
	}
	for _, k := range []string{"availableCredits", "credit", "balance", "available"} {
		if raw, ok := j.Data[k]; ok && len(raw) > 0 {
			credit = strings.Trim(string(raw), `"`)
			break
		}
	}
	plan = "妙手 CatPaw"
	if raw, ok := j.Data["userPlan"]; ok {
		var p struct {
			PlanName string `json:"planName"`
		}
		if json.Unmarshal(raw, &p) == nil && p.PlanName != "" {
			plan = p.PlanName
		}
	}
	return credit, plan, nil
}

// ===================== 聊天：round → event → turn =====================

// Message 是统一格式的一条消息（只支持纯文本内容）。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResult 是一次问答的结果。
//
// TurnComplete 只在收到 [DONE] 且帧全部可解析时为 true —— 上游 EOF 也能读，
// 但那是「流断了」，不能当成正常结束。
type ChatResult struct {
	Text         string
	Reasoning    string
	TurnComplete bool
}

// CheckSupported 在发任何网络请求之前，把「这段扁平文本协议表达不了」的请求挡掉。
//
// 工具、工具历史、非文本内容一律拒绝；悄悄丢掉会导致模型看不到用户实际发的东西。
func CheckSupported(msgs []Message, hasTools bool) error {
	if hasTools {
		return fmt.Errorf("%w: 妙手不支持工具", ErrUnsupported)
	}
	if len(msgs) == 0 {
		return fmt.Errorf("%w: 妙手需要消息", ErrUnsupported)
	}
	for _, m := range msgs {
		switch m.Role {
		case "system", "developer", "user", "assistant":
		default:
			return fmt.Errorf("%w: 妙手不支持此消息角色 %q", ErrUnsupported, m.Role)
		}
	}
	return nil
}

// BuildPrompt 把 messages 压成协议唯一能承载的那一段文本（对应插件的 buildPrompt）。
//
// 只有一条 user 消息时原样返回，不加角色前缀 —— 和真客户端一样。
func BuildPrompt(msgs []Message) string {
	if len(msgs) == 1 && msgs[0].Role == "user" {
		return msgs[0].Content
	}
	var b strings.Builder
	for _, m := range msgs {
		var block string
		switch m.Role {
		case "system", "developer":
			block = "[System Instructions]\n" + m.Content
		case "user":
			block = "[User]\n" + m.Content
		case "assistant":
			block = "[Assistant]\n" + m.Content
		default:
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(block)
	}
	return b.String()
}

// Chat 走完三步会话机，返回文本与推理文本。
//
// modelType 取自模型表；未知模型回落到 longcat-2.0 的 type（77）。
func Chat(ctx context.Context, acc *Account, model string, msgs []Message) (*ChatResult, error) {
	return ChatWithModelType(ctx, acc, model, 0, msgs)
}

// ChatWithModelType 允许传「目录接口给出的可信 modelType」，避免不认识的新模型被压成 77。
// trustedModelType <= 0 时按模型表推断。
func ChatWithModelType(ctx context.Context, acc *Account, model string, trustedModelType int, msgs []Message) (*ChatResult, error) {
	if acc == nil || acc.Token == "" {
		return nil, ErrSignInRequired
	}
	if err := CheckSupported(msgs, false); err != nil {
		return nil, err
	}

	modelType := ModelTypeOf(model)
	if trustedModelType > 0 {
		modelType = trustedModelType
	}
	convID := uuidLike()
	msgID := uuidLike()
	turnID := uuidLike()
	prompt := BuildPrompt(msgs)

	userMsg := func() map[string]any {
		return map[string]any{
			"type":      "user",
			"messageId": msgID,
			"content": []any{
				map[string]any{"type": "text", "text": prompt},
			},
			"finished": true,
		}
	}

	// ① round：建会话
	round := map[string]any{
		"conversationId": convID,
		"message":        userMsg(),
		"modelType":      modelType,
		"source":         Source,
		"mode":           Mode,
		"permissionMode": PermissionMode,
		"toolVersion":    ToolVersion,
		"repositories":   []any{},
	}
	if err := postCodeZero(ctx, Origin+PathRound, spanHeaders(acc, "0.2", "conversation.round"), round); err != nil {
		return nil, err
	}

	// ② event：标记会话运行中
	event := map[string]any{
		"conversationId": convID,
		"eventType":      "conversation",
		"data":           map[string]any{"status": "running"},
	}
	if err := postCodeZero(ctx, Origin+PathEvent, spanHeaders(acc, "0.3", "conversation.event"), event); err != nil {
		return nil, err
	}

	// ③ turn：SSE 答案
	turn := map[string]any{
		"conversationId": convID,
		"turnRequestId":  turnID,
		"message":        userMsg(),
		"modelType":      modelType,
		"source":         Source,
		"mode":           Mode,
		"permissionMode": PermissionMode,
		"toolConfigs":    []any{},
		"toolVersion":    ToolVersion,
	}
	h := spanHeaders(acc, "0.4.1", "conversation.turn")
	h["Accept"] = "text/event-stream"
	h["enableheartbeat"] = "true"
	body, err := postRaw(ctx, Origin+PathTurn, h, turn, 0)
	if err != nil {
		return nil, err
	}
	return ParseTurn(body)
}

// ParseTurn 解析 turn 的 SSE 响应体。
//
// 上游帧是「累积式」的：每帧给的是到目前为止的全文，所以后一帧直接覆盖前一帧，
// 不需要自己拼 delta。帧结构：
//
//	data: {"message":{"content":[{"type":"text","text":"..."},
//	                             {"type":"text","reasoningContent":"..."}]}}
func ParseTurn(body []byte) (*ChatResult, error) {
	res := &ChatResult{}
	var text, reasoning strings.Builder
	contentSeen := false
	done := false
	malformed := false
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			done = true
			break
		}
		var frame struct {
			Error   json.RawMessage `json:"error"`
			Message *struct {
				Content []struct {
					Type             string  `json:"type"`
					Text             *string `json:"text"`
					ReasoningContent *string `json:"reasoningContent"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			malformed = true
			continue
		}
		if len(frame.Error) > 0 && string(frame.Error) != "null" {
			var e struct {
				Code int    `json:"code"`
				Msg  string `json:"message"`
			}
			_ = json.Unmarshal(frame.Error, &e)
			return nil, fmt.Errorf("catpaw: 上游返回错误 code=%d %s", e.Code, e.Msg)
		}
		if frame.Message == nil {
			continue
		}
		for _, c := range frame.Message.Content {
			if c.Type != "text" {
				continue
			}
			if c.Text != nil {
				text.Reset()
				text.WriteString(*c.Text)
				contentSeen = true
			}
			if c.ReasoningContent != nil {
				reasoning.Reset()
				reasoning.WriteString(*c.ReasoningContent)
				contentSeen = true
			}
		}
	}
	if !contentSeen {
		return nil, errors.New("catpaw: turn 没有任何内容帧")
	}
	res.Text = text.String()
	res.Reasoning = reasoning.String()
	res.TurnComplete = done && !malformed
	return res, nil
}

// ===================== HTTP 底座 =====================

// UpstreamError 带上 HTTP 状态与业务 code，方便上层决定是否换账号。
type UpstreamError struct {
	Op      string
	Status  int
	Code    int
	Message string
}

// Error 实现 error。
func (e *UpstreamError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("catpaw: %s 失败 (http=%d code=%d): %s", e.Op, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("catpaw: %s 失败 (http=%d): %s", e.Op, e.Status, e.Message)
}

var errEmptyBody = errors.New("catpaw: 空响应")

func shortErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func getRaw(ctx context.Context, u string, h map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	return send(req)
}

func getJSON(ctx context.Context, u string, h map[string]string, out any) error {
	body, err := getRaw(ctx, u, h)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errEmptyBody
	}
	return json.Unmarshal(body, out)
}

// postRaw 发 POST，返回响应体。timeout<=0 时不设额外限制（靠 ctx）。
func postRaw(ctx context.Context, u string, h map[string]string, payload any, timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	return send(req)
}

// postCodeZero 发 POST 并要求 {"code":0}。
func postCodeZero(ctx context.Context, u string, h map[string]string, payload any) error {
	body, err := postRaw(ctx, u, h, payload, 30*time.Second)
	if err != nil {
		return err
	}
	var j struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return &UpstreamError{Op: "post", Status: 502, Message: "响应不是 JSON: " + shortErr(string(body))}
	}
	if j.Code == nil {
		return &UpstreamError{Op: "post", Status: 502, Message: "响应缺少 code: " + shortErr(string(body))}
	}
	if *j.Code != 0 {
		status := 502
		switch *j.Code {
		case 401, 403, 429:
			status = *j.Code
		}
		return &UpstreamError{Op: "post", Status: status, Code: *j.Code, Message: j.Message}
	}
	return nil
}

func send(req *http.Request) ([]byte, error) {
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &UpstreamError{
			Op:      "http",
			Status:  resp.StatusCode,
			Message: shortErr(string(body)),
		}
	}
	return body, nil
}

// ===================== 小工具 =====================

// randomHex 生成 n 字节的随机 hex（16 字节 = 32 个 hex 位，与 Java 的 hex32 对齐）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// uuidLike 生成 8-4-4-4-12 形态的随机 id（上游把它当 UUID 用）。
func uuidLike() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		b = make([]byte, 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
