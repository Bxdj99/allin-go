// Package codex 适配 OpenAI Codex / ChatGPT 订阅的「后端 API」协议
// （chatgpt.com/backend-api/codex），协议从 All-in 的 CodexAuth / CodexRequestPolicy /
// ResponsesBridge 三份 Java 实现里提取。
//
// 它和 api.openai.com 的常规 OpenAI API 有四处本质差别：
//
//  1. 鉴权是 ChatGPT 订阅的 OAuth token，不是 API Key。
//     Authorization: Bearer <access_token>，还要额外带 chatgpt-account-id
//     （来自 JWT 里 "https://api.openai.com/auth".chatgpt_account_id）。
//     只有 token 不够：originator / User-Agent 必须冒充官方 codex_cli_rs 客户端，
//     少一个就是 401/403。
//  2. 端点是 Responses API，不是 /v1/chat/completions：
//     POST https://chatgpt.com/backend-api/codex/responses
//     （另有 /responses/compact 压缩历史、/codex/models 模型目录、/wham/usage 额度）
//  3. 请求体是 Responses 结构（input[] + instructions + 扁平化 tools），
//     不是 chat 的 messages[]；上游只接受流式，stream 必须 true、store 必须 false。
//  4. 会话粘性靠两个头：session-id / thread-id（不是 body 字段）。body 里的
//     prompt_cache_key 优先，其次沿用客户端给的头，最后才由本包按对话内容推导。
//
// 认证链路（与官方 Codex CLI 完全一致）：
//
//	GET  https://auth.openai.com/oauth/authorize?...PKCE...  → 浏览器授权 → code
//	POST https://auth.openai.com/oauth/token (form-urlencoded) → access/id/refresh token
//	POST https://auth.openai.com/oauth/token (json, grant_type=refresh_token) → 续期
//
// 本包不接线、不落盘，只提供协议层能力：凭据解析、头构造、请求体转换、流解析。
package codex

import (
	"bufio"
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
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 协议常量。全部对应 Java 侧 CodexAuth 里硬编码的那一套，改动前先核对上游是否已变。
const (
	// ProviderID 是供应商 id（配置里的 providers[].id）。
	ProviderID = "codex"

	// ClientID 是 Codex CLI 官方 OAuth App 的 client id（公开常量）。
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// TokenURL 换 token / 续期都用这一个地址。
	TokenURL = "https://auth.openai.com/oauth/token"
	// AuthorizeURL 浏览器授权页。
	AuthorizeURL = "https://auth.openai.com/oauth/authorize"
	// RedirectURI 是 Codex CLI 固定回环地址；本包不监听该端口，只按回调链接里的 code 换票。
	RedirectURI = "http://localhost:1455/auth/callback"
	// CallbackPort 回调端口，签名与 Java 侧保持一致。
	CallbackPort = 1455
	// Scopes 必须原样带上，缺 offline_access 拿不到 refresh_token。
	Scopes = "openid profile email offline_access api.connectors.read api.connectors.invoke"

	// CodexBase 后端 API 根。
	CodexBase = "https://chatgpt.com/backend-api/codex"
	// CodexResponses 推理端点（Responses API）。
	CodexResponses = "https://chatgpt.com/backend-api/codex/responses"
	// CodexModels 模型目录，需要 ?client_version=。
	CodexModels = "https://chatgpt.com/backend-api/codex/models"
	// WhamUsage 额度/限额查询。
	WhamUsage = "https://chatgpt.com/backend-api/wham/usage"

	// CodexVersion 伪装客户端版本，同时也是 /codex/models 的 client_version 参数。
	CodexVersion = "0.159.0"
	// Originator 是官方客户端的来源标识，上游按它分桶。
	Originator = "codex_cli_rs"
	// UserAgent 必须和 Originator 自洽，否则被判定为非官方客户端。
	UserAgent = "codex_cli_rs/0.159.0 (Linux; aarch64) xterm-256color"
	// OpenAIBeta Responses API 的 experimental 标记，缺了会 404。
	OpenAIBeta = "responses=experimental"
)

// DefaultModels 是模型目录拉取失败时的兜底清单（对应 Java 的 DEFAULT_MODELS）。
var DefaultModels = []string{
	"gpt-5-codex",
	"gpt-5-codex-mini",
	"gpt-5",
	"gpt-5-mini",
	"gpt-5.1",
	"gpt-5.2",
	"o3",
	"o3-mini",
	"o1",
	"o1-mini",
	"o1-preview",
	"gpt-4o",
	"gpt-4o-mini",
	"gpt-4.5-preview",
}

// tokenRefreshLeeway 提前续期余量：剩余不足 5 分钟就当过期。
const tokenRefreshLeeway = 5 * time.Minute

// maxBody 单次响应体读取上限，避免上游异常时吃满内存。
const maxBody = 1 << 20

// HTTPClient 是默认客户端。鉴权/目录/额度这类小请求用它；转发推理请求时调用方可自带。
var HTTPClient = &http.Client{Timeout: 60 * time.Second}

// ErrStreamDone 由 ReadStream 内部使用，表示已经读到 terminal 事件。
var errStreamDone = errors.New("codex: stream done")

// Account 是一个 Codex 账号的凭据集合，字段与 All-in 的 config.providers[].accounts[]
// 一一对应（JSON tag 保持 Java 侧的驼峰命名）。
type Account struct {
	// ID 是池内唯一 id（形如 codex_<uuid>）。
	ID string `json:"id,omitempty"`
	// Name 是面板显示名，通常是 email。
	Name string `json:"name,omitempty"`
	// Type 目前只有 oauth / apikey 两种。
	Type string `json:"type,omitempty"`
	// AuthMode 与 Type 同义，Java 里叫 authMode。
	AuthMode string `json:"authMode,omitempty"`
	// Email 从 JWT 或登录信息里取。
	Email string `json:"email,omitempty"`
	// Plan 是 chatgpt_plan_type（plus / pro / team / free / chatgpt）。
	Plan string `json:"plan,omitempty"`
	// Access 是真正发给上游的 access_token。
	Access string `json:"access,omitempty"`
	// Token 与 Access 同值，兼容只写 token 的老配置。
	Token string `json:"token,omitempty"`
	// Refresh 是 refresh_token，续期用。
	Refresh string `json:"refresh,omitempty"`
	// IDToken 携带 chatgpt_account_id / plan，解析账号身份时优先用它。
	IDToken string `json:"idToken,omitempty"`
	// AccountID 就是 chatgpt_account_id，必须进 chatgpt-account-id 头。
	AccountID string `json:"accountId,omitempty"`
	// Expires 是 access_token 到期毫秒时间戳。
	Expires int64 `json:"expires,omitempty"`
	// AddedAt / UpdatedAt 是入池与更新时间（毫秒）。
	AddedAt   int64 `json:"addedAt,omitempty"`
	UpdatedAt int64 `json:"updatedAt,omitempty"`
	// Enabled 为 false 的账号不参与调度。
	Enabled bool `json:"enabled,omitempty"`
}

// AccessToken 返回当前手里的 access_token（Access 优先，回退 Token）。
func (a *Account) AccessToken() string {
	if a == nil {
		return ""
	}
	if a.Access != "" {
		return a.Access
	}
	return a.Token
}

// IsAPIKey 判断是不是 "sk-" 这种经典 API Key 账号（那种不走 ChatGPT 后端）。
func (a *Account) IsAPIKey() bool {
	if a == nil {
		return false
	}
	return a.AuthMode == "apikey" || a.Type == "apikey" || strings.HasPrefix(a.AccessToken(), "sk-")
}

// Expired 判断 access_token 是否已过期（Expires 为 0 视为未知，按未过期处理）。
func (a *Account) Expired() bool {
	if a == nil || a.Expires <= 0 {
		return false
	}
	return time.Now().UnixMilli() >= a.Expires
}

// NeedsRefresh 判断是否该主动续期：剩余不足 5 分钟即视为需要。
func (a *Account) NeedsRefresh() bool {
	if a == nil || a.Expires <= 0 || a.Refresh == "" {
		return false
	}
	return a.Expires-time.Now().UnixMilli() < tokenRefreshLeeway.Milliseconds()
}

// ---------------------------------------------------------------------------
// 凭据解析
// ---------------------------------------------------------------------------

// DecodeJWT 解出 JWT 的 payload（不验签，只做协议侧取值）。失败返回 nil。
func DecodeJWT(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return nil
	}
	return claims
}

// decodeBase64URL 兼容带/不带 padding 的 base64url（JWT 段经常省 padding）。
func decodeBase64URL(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	// 少数导出工具给出的是标准 base64
	s = strings.NewReplacer("-", "+", "_", "/").Replace(s)
	if m := len(s) % 4; m > 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(s)
}

// ParseCredentials 把用户粘贴的凭据文本解析成 Account。支持四种输入：
//
//  1. ~/.codex/auth.json（{"tokens":{...}}）
//  2. Cockpit Tools / Sub2API / CLIProxyAPI 导出的平铺 JSON
//  3. 裸 refresh_token（不含两个点）
//  4. 裸 access_token / id_token（JWT，含两个点），或 "Bearer xxx"
//  5. sk- 开头的 API Key（authMode=apikey，不走 ChatGPT 后端）
func ParseCredentials(raw string) (*Account, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, errors.New("codex: 凭据文本为空")
	}
	acc := &Account{AuthMode: "chatgpt", Type: "oauth"}

	if strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}") {
		obj, err := decodeObject([]byte(text))
		if err != nil {
			return nil, fmt.Errorf("codex: 凭据 JSON 无法解析: %w", err)
		}
		// 标准 auth.json 把 token 放在 tokens 子对象里，导出工具则常常是平铺的。
		src := obj
		if toks, ok := obj["tokens"].(map[string]any); ok {
			src = toks
		}
		acc.Access = pickString(src, "access_token")
		acc.Refresh = pickString(src, "refresh_token")
		acc.IDToken = pickString(src, "id_token")
		acc.AccountID = pickString(src, "account_id")
		if v := pickString(obj, "email"); v != "" {
			acc.Email = v
		}
		if v := pickString(obj, "plan_type"); v != "" {
			acc.Plan = v
		}
		if v := pickString(obj, "auth_mode"); v != "" {
			acc.AuthMode = v
		}
	} else if strings.HasPrefix(text, "sk-") {
		acc.Access = text
		acc.AuthMode = "apikey"
		acc.Type = "apikey"
		n := len(text)
		if n > 10 {
			n = 10
		}
		acc.Email = "API-Key-" + text[:n] + "..."
		return acc, nil
	} else {
		text = strings.TrimSpace(strings.TrimPrefix(text, "Bearer "))
		// 两个点 = JWT（access/id token）；否则按 refresh_token 处理。
		if strings.Count(text, ".") == 2 {
			acc.Access = text
		} else {
			acc.Refresh = text
		}
	}

	// 身份信息一律从 JWT 里取：id_token 优先（它带 chatgpt_account_id），否则 access_token。
	jwt := acc.IDToken
	if jwt == "" {
		jwt = acc.Access
	}
	if claims := DecodeJWT(jwt); claims != nil {
		if acc.Email == "" {
			acc.Email = pickString(claims, "email")
		}
		if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			if acc.AccountID == "" {
				acc.AccountID = pickString(auth, "chatgpt_account_id")
			}
			if acc.Plan == "" {
				if v := pickString(auth, "chatgpt_plan_type"); v != "" {
					acc.Plan = v
				}
			}
		}
		if acc.Plan == "" {
			acc.Plan = "chatgpt"
		}
		if exp := pickInt(claims, "exp"); exp > 0 {
			acc.Expires = exp * 1000 // JWT 的 exp 是秒
		}
	}
	if acc.Plan == "" {
		acc.Plan = "chatgpt"
	}
	if acc.Email == "" {
		if acc.AccountID != "" {
			short := acc.AccountID
			if len(short) > 8 {
				short = short[:8]
			}
			acc.Email = "Codex-" + short
		} else {
			acc.Email = "ChatGPT-User"
		}
	}
	acc.Enabled = true
	return acc, nil
}

// ---------------------------------------------------------------------------
// 请求头
// ---------------------------------------------------------------------------

// ClientHeaders 只给「官方客户端指纹」这三个头（对应 Java 的 CodexAuth.clientHeaders）。
func ClientHeaders(accountID string) map[string]string {
	h := map[string]string{
		"originator": Originator,
		"User-Agent": UserAgent,
	}
	if accountID != "" {
		h["chatgpt-account-id"] = accountID
	}
	return h
}

// Headers 返回调用 /backend-api/codex/* 所需的完整头集合。
//
// 关键点：Authorization 用 ChatGPT 订阅的 access_token（不是 API Key）；
// chatgpt-account-id 必须与 token 里的 chatgpt_account_id 一致，
// 否则上游会把它当成「另一个账号」而返回 401/403。
func Headers(acc *Account) map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + acc.AccessToken(),
		"Accept":        "text/event-stream, application/json",
		"Content-Type":  "application/json",
		"originator":    Originator,
		"User-Agent":    UserAgent,
		"OpenAI-Beta":   OpenAIBeta,
	}
	if acc != nil && acc.AccountID != "" {
		h["chatgpt-account-id"] = acc.AccountID
	}
	return h
}

// UsageHeaders 是额度/模型目录这类 GET 请求的头（只认 json，不要 event-stream）。
func UsageHeaders(acc *Account) map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + acc.AccessToken(),
		"Accept":        "application/json",
		"originator":    Originator,
		"User-Agent":    UserAgent,
	}
	if acc != nil && acc.AccountID != "" {
		h["chatgpt-account-id"] = acc.AccountID
	}
	return h
}

// SessionHeaders 生成会话粘性头 session-id / thread-id。
//
// 优先级（与 Java 的 CodexRequestPolicy.sessionHeaders 一致）：
//  1. body 里有 prompt_cache_key → 直接用它；
//  2. 客户端自己给了 session-id / thread-id → 原样沿用（客户端命名的对话不能被覆盖）；
//  3. 都没有 → 用 instructions + 到首条 user 消息为止的 input 做 SHA-256，
//     拼成 UUID 形状（前 32 个 hex 字符），保证同一轮对话的每一 turn 名字相同。
//
// 只产出带连字符的这两个头，下划线那套老名字一律不发。
func SessionHeaders(body []byte, inbound map[string]string) map[string]string {
	out := map[string]string{}
	var obj map[string]any
	if len(body) > 0 {
		var err error
		if obj, err = decodeObject(body); err != nil {
			obj = nil
		}
	}
	if obj != nil {
		if key := pickString(obj, "prompt_cache_key"); key != "" {
			return namedSession(out, key)
		}
	}
	session := strings.TrimSpace(inbound["session-id"])
	thread := strings.TrimSpace(inbound["thread-id"])
	if session != "" || thread != "" {
		if session != "" {
			out["session-id"] = validSessionValue(session)
		}
		if thread != "" {
			out["thread-id"] = validSessionValue(thread)
		}
		return out
	}
	if obj == nil {
		return out
	}
	if derived := ConversationKey(obj); derived != "" {
		return namedSession(out, derived)
	}
	return out
}

// namedSession 把同一个名字同时写进 session-id 与 thread-id。
func namedSession(h map[string]string, session string) map[string]string {
	v := validSessionValue(session)
	h["session-id"] = v
	h["thread-id"] = v
	return h
}

// validSessionValue 做最基本的头注入防御（Java 侧是长度 + CRLF 检查）。
func validSessionValue(s string) string {
	if len(s) > 2048 || strings.ContainsAny(s, "\r\n") {
		return ""
	}
	return s
}

// ConversationKey 计算一次对话的稳定标识（UUID 形状）。
//
// 算法：SHA-256(instructions ‖ 逐项 canonical(input) 到首条 user 消息为止)，
// canonical 时丢掉 item 的 id、以及 message/reasoning 的 status ——
// 这类字段客户端每轮都会重编号，不能让它挪动会话名。
func ConversationKey(body map[string]any) string {
	input, ok := body["input"]
	if !ok {
		return ""
	}
	if s, isStr := input.(string); isStr {
		// 字符串 input 是 final shaping 之前的形态，先按 shaping 结果包一层再算
		input = []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": s}},
		}}
	}
	items, ok := input.([]any)
	if !ok {
		return ""
	}
	digest := sha256.New()
	digest.Write([]byte(pickString(body, "instructions")))
	hasUser := false
	for _, raw := range items {
		item, isObj := raw.(map[string]any)
		if isObj && pickString(item, "type") == "item_reference" {
			continue // item_reference 在上游取会话键之前就被丢掉
		}
		var buf strings.Builder
		canonicalize(raw, &buf)
		digest.Write([]byte(buf.String()))
		if isObj && pickString(item, "role") == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		return "" // 还没有用户消息，就不给对话起名
	}
	hexSum := hex.EncodeToString(digest.Sum(nil))
	return hexSum[0:8] + "-" + hexSum[8:12] + "-" + hexSum[12:16] + "-" +
		hexSum[16:20] + "-" + hexSum[20:32]
}

// canonicalize 输出确定性 JSON（键排序），丢掉 id 与 message/reasoning 的 status。
func canonicalize(value any, out *strings.Builder) {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case map[string]any:
		kind := pickString(v, "type")
		names := make([]string, 0, len(v))
		for name := range v {
			if name == "id" {
				continue
			}
			if name == "status" && (kind == "" || kind == "message" || kind == "reasoning") {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)
		out.WriteByte('{')
		for i, name := range names {
			if i > 0 {
				out.WriteByte(',')
			}
			out.WriteString(quoteJSON(name))
			out.WriteByte(':')
			canonicalize(v[name], out)
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			canonicalize(item, out)
		}
		out.WriteByte(']')
	case string:
		out.WriteString(quoteJSON(v))
	case json.Number:
		out.WriteString(v.String())
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case float64:
		b, _ := json.Marshal(v)
		out.Write(b)
	default:
		out.WriteString(quoteJSON(fmt.Sprint(v)))
	}
}

// quoteJSON 用标准库做安全的字符串转义。
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 请求体：Chat Completions → Codex Responses
// ---------------------------------------------------------------------------

// chatAllowed 是 Chat 侧允许进入转换的字段白名单，多一个直接报错（避免静默丢参数）。
var chatAllowed = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"user": true, "safety_identifier": true, "max_tokens": true,
	"max_completion_tokens": true, "temperature": true, "top_p": true,
	"parallel_tool_calls": true, "metadata": true, "prompt_cache_key": true,
	"service_tier": true, "reasoning_effort": true, "tools": true,
	"tool_choice": true, "response_format": true, "n": true,
}

// 输出上限相关的字段名。
var (
	// outputLimitFields 会做「必须正整数」校验的三个别名。
	outputLimitFields = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}
	// codexRemovedOutputFields Codex 后端不认，必须删掉。
	codexRemovedOutputFields = []string{"max_completion_tokens", "max_output_tokens"}
	// codexRemovedFields 上游明确不接受的采样/追踪字段。
	codexRemovedFields = []string{
		"temperature", "top_p", "previous_response_id", "user",
		"safety_identifier", "prompt_cache_options",
	}
)

// BuildChatRequest 把 OpenAI 风格的 Chat Completions 请求体转换成可以直接 POST 给
// https://chatgpt.com/backend-api/codex/responses 的 Responses 请求体。
//
// 两步：先把 messages[] 摊平成 input[]，再做 Codex 专属收尾（删字段、强制 stream/store）。
// upstreamModel 是最终发给上游的模型名（调用方负责把客户端模型映射过来）。
func BuildChatRequest(chatBody []byte, upstreamModel string) ([]byte, error) {
	chat, err := decodeObject(chatBody)
	if err != nil {
		return nil, fmt.Errorf("codex: Chat 请求体无法解析: %w", err)
	}
	resp, err := ChatToResponses(chat, upstreamModel)
	if err != nil {
		return nil, err
	}
	final, err := finalizeResponses(resp)
	if err != nil {
		return nil, err
	}
	return json.Marshal(final)
}

// ChatToResponses 只做结构转换，不做 Codex 收尾（对应 Java 的 ResponsesBridge.chatToResponses）。
func ChatToResponses(chat map[string]any, model string) (map[string]any, error) {
	for key := range chat {
		if !chatAllowed[key] {
			return nil, fmt.Errorf("codex: Chat 字段 %q 无法映射到 Responses", key)
		}
	}
	if n, ok := chat["n"]; ok {
		if ni, err := asInt(n); err != nil || ni != 1 {
			return nil, errors.New("codex: Responses 只支持一个 choice（n 必须为 1）")
		}
	}
	if so, ok := chat["stream_options"]; ok && so != nil {
		opts, isObj := so.(map[string]any)
		if !isObj {
			return nil, errors.New("codex: stream_options 必须是对象")
		}
		for _, key := range []string{"include_usage", "include_obfuscation"} {
			if v, has := opts[key]; has {
				if _, isBool := v.(bool); !isBool {
					return nil, fmt.Errorf("codex: stream_options.%s 必须是布尔", key)
				}
			}
		}
	}

	out := map[string]any{"model": model}
	copyField(chat, out, "stream", "stream")
	copyField(chat, out, "user", "user")
	copyField(chat, out, "safety_identifier", "safety_identifier")
	copyField(chat, out, "max_tokens", "max_output_tokens")
	if opts, ok := chat["stream_options"].(map[string]any); ok {
		if v, has := opts["include_obfuscation"]; has {
			out["stream_options"] = map[string]any{"include_obfuscation": v}
		}
	}
	// max_completion_tokens 更贴近 Responses 语义：比 max_tokens 小的时候覆盖它。
	if v, ok := chat["max_completion_tokens"]; ok {
		if c, err := asInt(v); err == nil && c > 0 {
			cur := int64(0)
			if m, has := out["max_output_tokens"]; has {
				cur, _ = asInt(m)
			}
			if cur <= 0 || c < cur {
				out["max_output_tokens"] = json.Number(fmt.Sprint(c))
			}
		}
	}
	copyField(chat, out, "temperature", "temperature")
	copyField(chat, out, "top_p", "top_p")
	copyField(chat, out, "parallel_tool_calls", "parallel_tool_calls")
	copyField(chat, out, "metadata", "metadata")
	copyField(chat, out, "prompt_cache_key", "prompt_cache_key")
	copyField(chat, out, "service_tier", "service_tier")
	if v, ok := chat["reasoning_effort"]; ok && v != nil {
		out["reasoning"] = map[string]any{"effort": v}
	}

	messages, ok := chat["messages"].([]any)
	if !ok {
		return nil, errors.New("codex: messages 必填且必须是数组")
	}

	var input []any
	var instructions strings.Builder
	for i, raw := range messages {
		m, isObj := raw.(map[string]any)
		if !isObj {
			return nil, fmt.Errorf("codex: messages[%d] 不是对象", i)
		}
		for key := range m {
			switch key {
			case "role", "content", "tool_calls", "tool_call_id", "reasoning_content":
			default:
				return nil, fmt.Errorf("codex: messages[%d].%s 无法映射", i, key)
			}
		}
		role := pickString(m, "role")

		// 历史里的 reasoning_content 要还原成独立 reasoning item，否则思考链会丢。
		if rc, has := m["reasoning_content"]; has && rc != nil {
			text, isStr := rc.(string)
			if role != "assistant" || !isStr {
				return nil, errors.New("codex: reasoning_content 只允许出现在 assistant 消息里且必须是字符串")
			}
			input = append(input, map[string]any{
				"type": "reasoning",
				"summary": []any{
					map[string]any{"type": "summary_text", "text": text},
				},
			})
		}

		switch role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("codex: 不支持的 role %q", role)
		}

		// 开头连续的 system/developer 提成 instructions（Responses 没有 system message）。
		if (role == "system" || role == "developer") && len(input) == 0 {
			if s, isStr := m["content"].(string); isStr {
				if instructions.Len() > 0 {
					instructions.WriteString("\n\n")
				}
				instructions.WriteString(s)
				continue
			}
		}

		if role == "tool" {
			s, isStr := m["content"].(string)
			if !isStr {
				return nil, errors.New("codex: tool 消息的 content 必须是字符串")
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": requiredString(m, "tool_call_id"),
				"output":  s,
			})
			continue
		}

		if content, has := m["content"]; has && content != nil {
			parts, err := convertContent(content, role)
			if err != nil {
				return nil, fmt.Errorf("codex: messages[%d]: %w", i, err)
			}
			if len(parts) > 0 {
				input = append(input, map[string]any{
					"type": "message", "role": role, "content": parts,
				})
			}
		}

		if callsRaw, has := m["tool_calls"]; has && callsRaw != nil {
			calls, isArr := callsRaw.([]any)
			if !isArr {
				return nil, fmt.Errorf("codex: messages[%d].tool_calls 必须是数组", i)
			}
			seen := map[string]bool{}
			for j, cRaw := range calls {
				c, isObj := cRaw.(map[string]any)
				if !isObj {
					return nil, fmt.Errorf("codex: messages[%d].tool_calls[%d] 不是对象", i, j)
				}
				if pickString(c, "type") != "function" {
					return nil, fmt.Errorf("codex: messages[%d].tool_calls[%d] 不是 function 调用", i, j)
				}
				id := requiredString(c, "id")
				if seen[id] {
					return nil, fmt.Errorf("codex: messages[%d].tool_calls 里 call id 重复", i)
				}
				seen[id] = true
				fn, isObj := c["function"].(map[string]any)
				if !isObj {
					return nil, fmt.Errorf("codex: messages[%d].tool_calls[%d].function 缺失", i, j)
				}
				args, err := toolArguments(fn)
				if err != nil {
					return nil, err
				}
				input = append(input, map[string]any{
					"type": "function_call", "call_id": id,
					"name": requiredString(fn, "name"), "arguments": args,
				})
			}
		}
	}
	out["input"] = input
	if instructions.Len() > 0 {
		out["instructions"] = instructions.String()
	}

	if toolsRaw, has := chat["tools"]; has && toolsRaw != nil {
		tools, isArr := toolsRaw.([]any)
		if !isArr {
			return nil, errors.New("codex: tools 必须是数组")
		}
		converted := make([]any, 0, len(tools))
		for i, tRaw := range tools {
			t, isObj := tRaw.(map[string]any)
			if !isObj {
				return nil, fmt.Errorf("codex: tools[%d] 不是对象", i)
			}
			if pickString(t, "type") != "function" {
				return nil, fmt.Errorf("codex: tools[%d] 只支持 function 类型", i)
			}
			fn, isObj := t["function"].(map[string]any)
			if !isObj {
				return nil, fmt.Errorf("codex: tools[%d].function 缺失", i)
			}
			// Responses 的工具是扁平的：{type:function, name, description, parameters}
			flat := cloneMap(fn)
			flat["type"] = "function"
			converted = append(converted, flat)
		}
		out["tools"] = converted
	}

	if choice, has := chat["tool_choice"]; has && choice != nil {
		switch c := choice.(type) {
		case string:
			out["tool_choice"] = c
		case map[string]any:
			fn, isObj := c["function"].(map[string]any)
			if !isObj {
				return nil, errors.New("codex: tool_choice.function 缺失")
			}
			out["tool_choice"] = map[string]any{"type": "function", "name": requiredString(fn, "name")}
		default:
			return nil, errors.New("codex: tool_choice 类型不支持")
		}
	}

	if format, has := chat["response_format"].(map[string]any); has {
		var f map[string]any
		if pickString(format, "type") == "json_schema" {
			schema, isObj := format["json_schema"].(map[string]any)
			if !isObj {
				return nil, errors.New("codex: response_format.json_schema 缺失")
			}
			f = cloneMap(schema)
			f["type"] = "json_schema"
		} else {
			f = cloneMap(format)
		}
		out["text"] = map[string]any{"format": f}
	}
	return out, nil
}

// convertContent 把 Chat 的 content（字符串或 parts 数组）转成 Responses 的 content parts。
func convertContent(content any, role string) ([]any, error) {
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	switch v := content.(type) {
	case string:
		return []any{map[string]any{"type": textType, "text": v}}, nil
	case []any:
		parts := make([]any, 0, len(v))
		for i, pRaw := range v {
			p, isObj := pRaw.(map[string]any)
			if !isObj {
				return nil, fmt.Errorf("content[%d] 不是对象", i)
			}
			switch pickString(p, "type") {
			case "text":
				parts = append(parts, map[string]any{"type": textType, "text": requiredString(p, "text")})
			case "image_url":
				img := p["image_url"]
				item := map[string]any{"type": "input_image"}
				switch im := img.(type) {
				case string:
					item["image_url"] = im
				case map[string]any:
					item["image_url"] = requiredString(im, "url")
					if d := pickString(im, "detail"); d != "" {
						item["detail"] = d
					}
				default:
					return nil, errors.New("image_url 结构非法")
				}
				parts = append(parts, item)
			default:
				return nil, fmt.Errorf("不支持的 content type %q", pickString(p, "type"))
			}
		}
		return parts, nil
	case nil:
		return nil, nil
	}
	return nil, errors.New("content 既不是字符串也不是数组")
}

// toolArguments 取 tool call 的 arguments 字符串（对象形式要序列化回字符串）。
func toolArguments(fn map[string]any) (string, error) {
	switch v := fn["arguments"].(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return "{}", nil
		}
		return v, nil
	case map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return "", errors.New("codex: tool call arguments 缺失或类型非法")
}

// ---------------------------------------------------------------------------
// Codex 专属收尾（对应 Java 的 CodexRequestPolicy.prepareFinalOutbound）
// ---------------------------------------------------------------------------

// FinalizeResponses 对「已经是 Responses 结构」的请求体做 Codex 收尾，可单独用于原生透传。
//
// 注意：Java 侧还有一段「Context Guard 输出上限无法保证就本地 400」的逻辑，
// 那属于网关策略、不属于上游协议，本包不带。
func FinalizeResponses(body []byte) ([]byte, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, fmt.Errorf("codex: Responses 请求体无法解析: %w", err)
	}
	final, err := finalizeResponses(obj)
	if err != nil {
		return nil, err
	}
	return json.Marshal(final)
}

// finalizeResponses 就地对副本做收尾，入参不会被修改。
func finalizeResponses(in map[string]any) (map[string]any, error) {
	body := cloneMap(in)

	// 输出上限先验合法性：上游对非正整数一律 400，这里提前报错更好定位。
	for _, field := range outputLimitFields {
		if v, has := body[field]; has {
			if _, err := positiveInt(v); err != nil {
				return nil, fmt.Errorf("codex: %s 必须是正整数: %w", field, err)
			}
		}
	}
	// Codex 后端只认 max_tokens（留不留都行），另两个别名必须删。
	for _, field := range codexRemovedOutputFields {
		delete(body, field)
	}
	// 上游不接受的采样/追踪字段（温度、top_p、用户标识、上轮 response id 等）。
	for _, field := range codexRemovedFields {
		delete(body, field)
	}
	// service_tier 只放行这两个值。
	if tier, ok := body["service_tier"].(string); !ok || (tier != "priority" && tier != "ultrafast") {
		delete(body, "service_tier")
	}

	// 上游只走流式，且不能存服务端（store=true 会被直接拒）。
	body["stream"] = true
	body["store"] = false
	if _, has := body["instructions"]; !has {
		body["instructions"] = ""
	}
	body["tool_choice"] = "auto"
	if _, has := body["parallel_tool_calls"]; !has {
		body["parallel_tool_calls"] = true
	}
	if _, has := body["text"]; !has {
		body["text"] = map[string]any{"verbosity": "medium"}
	}
	normalizeReasoning(body)
	normalizeStringInput(body)
	return body, nil
}

// normalizeReasoning 归一化推理档位：ultra→max、none 去掉 include、默认要加密推理、summary 默认 auto。
func normalizeReasoning(body map[string]any) {
	reasoning, _ := body["reasoning"].(map[string]any)
	effort := ""
	if reasoning != nil {
		effort = pickString(reasoning, "effort")
	}
	if effort == "ultra" && reasoning != nil {
		reasoning["effort"] = "max"
	}
	if effort == "none" {
		delete(body, "include")
		return
	}
	if reasoning != nil {
		if _, has := reasoning["summary"]; !has {
			reasoning["summary"] = "auto"
		}
	}
	include, _ := body["include"].([]any)
	found := false
	for _, item := range include {
		if s, ok := item.(string); ok && s == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		include = append(include, "reasoning.encrypted_content")
	}
	body["include"] = include
}

// normalizeStringInput 把字符串型 input 包成标准的 message item。
func normalizeStringInput(body map[string]any) {
	s, ok := body["input"].(string)
	if !ok {
		return
	}
	body["input"] = []any{map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": s}},
	}}
}

// ---------------------------------------------------------------------------
// OAuth（PKCE）与续期
// ---------------------------------------------------------------------------

// AuthFlow 是一次进行中的 PKCE 授权。
type AuthFlow struct {
	// FlowID 仅用于本地关联。
	FlowID string `json:"flowId"`
	// Verifier 是 PKCE code_verifier，换票时用，别外传。
	Verifier string `json:"-"`
	// State 防 CSRF。
	State string `json:"state"`
	// AuthURL 是给用户打开的授权地址。
	AuthURL string `json:"authUrl"`
	// CreatedAt 发起时间。
	CreatedAt time.Time `json:"createdAt"`
}

// StartAuth 生成一次 PKCE 授权：code_verifier 32 字节随机，challenge = base64url(SHA256(verifier))。
func StartAuth() (*AuthFlow, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("codex: 生成 PKCE verifier 失败: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("codex: 生成 state 失败: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", RedirectURI)
	q.Set("scope", Scopes)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	// 这两个是 Codex CLI 的私有参数：让授权页走精简流程并带上组织信息。
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", Originator)

	return &AuthFlow{
		FlowID:    fmt.Sprintf("flow_%d", time.Now().UnixMilli()),
		Verifier:  verifier,
		State:     state,
		AuthURL:   AuthorizeURL + "?" + q.Encode(),
		CreatedAt: time.Now(),
	}, nil
}

// ExchangeCode 用回调里的 code 换 token。注意这里是 form-urlencoded，不是 JSON。
func ExchangeCode(ctx context.Context, code, verifier string) (*Account, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", RedirectURI)
	form.Set("client_id", ClientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", Originator)
	req.Header.Set("User-Agent", UserAgent)

	body, status, err := doRequest(HTTPClient, req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("codex: 授权换票失败 (HTTP %d): %s", status, snippet(body))
	}
	var res struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("codex: 换票响应非法: %w", err)
	}
	acc := &Account{
		Access: res.AccessToken, Refresh: res.RefreshToken, IDToken: res.IDToken,
		AuthMode: "chatgpt", Type: "oauth", Enabled: true,
	}
	fillIdentityFromJWT(acc)
	if acc.Email == "" {
		acc.Email = "Codex-OAuth-User"
	}
	return acc, nil
}

// Refresh 用 refresh_token 续期，成功后写回 acc（对应 Java 的 refreshToken）。
//
// reason 会进 refresh_reason 字段（proactive_expiry / unauthorized_recovery / forced）。
func Refresh(ctx context.Context, acc *Account, reason string) error {
	if acc == nil || acc.Refresh == "" {
		return errors.New("codex: 无可用的 refresh_token，无法续期")
	}
	if reason == "" {
		reason = "proactive_expiry"
	}
	payload := map[string]any{
		"client_id":      ClientID,
		"grant_type":     "refresh_token",
		"refresh_token":  acc.Refresh,
		"refresh_reason": reason,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", Originator)
	req.Header.Set("User-Agent", UserAgent)

	body, status, err := doRequest(HTTPClient, req)
	if err != nil {
		return fmt.Errorf("codex: 令牌刷新失败：网络或响应错误")
	}
	if status != http.StatusOK {
		return fmt.Errorf("codex: 令牌刷新失败 (HTTP %d)", status)
	}
	var res struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("codex: 刷新响应非法: %w", err)
	}
	if res.AccessToken == "" {
		return errors.New("codex: 刷新响应中缺少 access_token")
	}
	acc.Access = res.AccessToken
	if res.RefreshToken != "" {
		acc.Refresh = res.RefreshToken
	}
	if res.IDToken != "" {
		acc.IDToken = res.IDToken
	}
	acc.UpdatedAt = time.Now().UnixMilli()
	fillIdentityFromJWT(acc)
	return nil
}

// EnsureFresh 在快过期时续期，供调用方在发请求前统一调用。
func EnsureFresh(ctx context.Context, acc *Account) error {
	if acc == nil || !acc.NeedsRefresh() {
		return nil
	}
	return Refresh(ctx, acc, "proactive_expiry")
}

// fillIdentityFromJWT 从 id_token（优先）或 access_token 补 email / accountId / plan / 到期时间。
func fillIdentityFromJWT(acc *Account) {
	jwt := acc.IDToken
	if jwt == "" {
		jwt = acc.Access
	}
	claims := DecodeJWT(jwt)
	if claims == nil {
		return
	}
	if acc.Email == "" || strings.HasPrefix(acc.Email, "Codex-") {
		if v := pickString(claims, "email"); v != "" {
			acc.Email = v
		}
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if acc.AccountID == "" {
			acc.AccountID = pickString(auth, "chatgpt_account_id")
		}
		if acc.Plan == "" || acc.Plan == "chatgpt" {
			if v := pickString(auth, "chatgpt_plan_type"); v != "" {
				acc.Plan = v
			}
		}
	}
	if exp := pickInt(claims, "exp"); exp > 0 {
		acc.Expires = exp * 1000
	}
}

// ---------------------------------------------------------------------------
// 额度与模型目录
// ---------------------------------------------------------------------------

// FetchUsage 查询套餐额度（Primary/Secondary RateLimit 与 Credits 余额），401 时自动续期重试一次。
func FetchUsage(ctx context.Context, acc *Account) (map[string]any, error) {
	if acc.IsAPIKey() {
		return map[string]any{"plan_type": "API Key", "status": "ok"}, nil
	}
	if err := EnsureFresh(ctx, acc); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, WhamUsage, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range UsageHeaders(acc) {
			req.Header.Set(k, v)
		}
		body, status, err := doRequest(HTTPClient, req)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized && attempt == 0 && acc.Refresh != "" {
			if err := Refresh(ctx, acc, "unauthorized_recovery"); err != nil {
				return nil, err
			}
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("codex: 额度查询失败 (HTTP %d)", status)
		}
		return decodeObject(body)
	}
	return nil, errors.New("codex: 额度查询仍然 401")
}

// Models 拉取线上可用模型（GET /backend-api/codex/models?client_version=0.159.0）。
// 返回空切片表示这次没拿到（调用方应保留自己的旧清单，不要以为上游没有模型）。
func Models(ctx context.Context, acc *Account) ([]string, error) {
	out := []string{}
	if acc == nil || acc.IsAPIKey() {
		return out, nil
	}
	if err := EnsureFresh(ctx, acc); err != nil {
		return out, err
	}
	endpoint := CodexModels + "?client_version=" + url.QueryEscape(CodexVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return out, err
	}
	for k, v := range UsageHeaders(acc) {
		req.Header.Set(k, v)
	}
	body, status, err := doRequest(HTTPClient, req)
	if err != nil {
		return out, err
	}
	if status != http.StatusOK {
		return out, nil
	}
	root, err := decodeObject(body)
	if err != nil {
		return out, nil
	}
	list, _ := root["models"].([]any)
	for _, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			return []string{}, nil
		}
		slug := strings.TrimSpace(pickString(m, "slug"))
		if slug == "" {
			return []string{}, nil
		}
		// visibility=hide 的模型不下发给客户端。
		if strings.EqualFold(pickString(m, "visibility"), "hide") {
			continue
		}
		duplicate := false
		for _, existing := range out {
			if existing == slug {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, slug)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 流解析（Responses SSE）
// ---------------------------------------------------------------------------

// StreamEvent 是一条 Responses SSE 事件。
type StreamEvent struct {
	// Type 事件名，例如 response.output_text.delta / response.completed。
	Type string
	// Raw 是事件原文的解析结果（已用 json.Number 保精度）。
	Raw map[string]any
}

// Delta 返回该事件的增量文本（只有 *.delta 事件才有）。
func (e StreamEvent) Delta() string {
	return pickString(e.Raw, "delta")
}

// IsTerminal 判断是不是终止事件。
func (e StreamEvent) IsTerminal() bool {
	switch e.Type {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	}
	return false
}

// ReadStream 逐条解析 Responses SSE。回调返回错误即中止。
// 读到 response.completed / response.incomplete 返回 nil；response.failed 与中途 EOF 返回错误。
//
// 上游会下发的关键事件：
//
//	response.output_text.delta           正文增量
//	response.reasoning_summary_text.delta 思考摘要增量
//	response.output_item.added/done      输出项（含 function_call）
//	response.completed / incomplete      终止
func ReadStream(r io.Reader, fn func(StreamEvent) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data strings.Builder
	terminated := false

	dispatch := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		payload := data.String()
		data.Reset()
		name := event
		event = ""
		if payload == "[DONE]" {
			return nil
		}
		obj, err := decodeObject([]byte(payload))
		if err != nil {
			return fmt.Errorf("codex: SSE 事件 JSON 非法: %w", err)
		}
		typ := pickString(obj, "type")
		if typ == "" {
			typ = name
		}
		if typ == "" {
			return errors.New("codex: SSE 事件缺少 type")
		}
		if typ == "error" {
			return fmt.Errorf("codex: 上游返回 error 事件: %s", snippet([]byte(payload)))
		}
		ev := StreamEvent{Type: typ, Raw: obj}
		if err := fn(ev); err != nil {
			return err
		}
		switch typ {
		case "response.completed", "response.incomplete":
			terminated = true
			return errStreamDone
		case "response.failed":
			return errors.New("codex: 上游返回 response.failed")
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			if len(line) > maxBody {
				return errors.New("codex: SSE 单行过长")
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if derr := dispatch(); derr != nil {
					if errors.Is(derr, errStreamDone) {
						return nil
					}
					return derr
				}
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(line[len("event:"):])
			case strings.HasPrefix(line, "data:"):
				v := strings.TrimPrefix(line[len("data:"):], " ")
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(v)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if data.Len() > 0 {
					if derr := dispatch(); derr != nil {
						if errors.Is(derr, errStreamDone) {
							return nil
						}
						return derr
					}
				}
				if !terminated {
					return errors.New("codex: 流在 terminal 事件前结束")
				}
				return nil
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// 通用小工具
// ---------------------------------------------------------------------------

// decodeObject 解 JSON 对象，保留数字原样（json.Number，避免 1e+06 之类的失真）。
func decodeObject(b []byte) (map[string]any, error) {
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// cloneMap 深拷贝一层 map（值里的嵌套对象直接复用，本包只做浅层改写）。
func cloneMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// copyField 源字段存在且非 nil 时拷到目标字段。
func copyField(src, dst map[string]any, from, to string) {
	if v, ok := src[from]; ok && v != nil {
		dst[to] = v
	}
}

// pickString 取字符串字段，缺失或类型不符返回空串。
func pickString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// requiredString 取必填字符串，缺失即报错。
func requiredString(m map[string]any, key string) string {
	return pickString(m, key)
}

// pickInt 取整数字段（兼容 json.Number / float64）。
func pickInt(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	if v, err := asInt(m[key]); err == nil {
		return v
	}
	return 0
}

// asInt 把任意 JSON 数字转 int64。
func asInt(v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		return n.Int64()
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	}
	return 0, errors.New("codex: 不是数字")
}

// positiveInt 校验正整数。
func positiveInt(v any) (int64, error) {
	n, err := asInt(v)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, errors.New("codex: 必须大于 0")
	}
	return n, nil
}

// doRequest 发请求并读完响应体（带 maxBody 上限）。
func doRequest(c *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// snippet 截断错误体，避免把整页 HTML 写进日志。
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
