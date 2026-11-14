// Package deepseekweb 适配 DeepSeek 网页版（chat.deepseek.com）的私有协议。
//
// 协议与 PoW 算法取自 All-in 的 DeepSeekWeb / DeepSeekWebChat / DeepSeekToolBridge，
// 上游溯源是 volcanicll 的 MIT 项目 magpie-deepseek-web-auth。
//
// 调用链路：
//
//	GET  /api/v0/users/current            校验 token（读 user.id/email）
//	POST /api/v0/chat_session/create      建会话 → data.biz_data.chat_session.id
//	POST /api/v0/chat/create_pow_challenge {"target_path":"/api/v0/chat/completion"}
//	                                      → 挑战 {algorithm,challenge,salt,signature,difficulty,expire_at}
//	POST /api/v0/chat/completion          SSE 增量（patch 语义）
//	POST /api/v0/chat/resume_stream       首轮没吐内容时重放
//	POST /api/v0/chat/continue            上游 INCOMPLETE 时续写
//	POST /api/v0/chat_session/delete      用完删会话
//
// 反爬机制：有，且只有 PoW 一种（没有签名、没有 turnstile、没有设备指纹）：
//   - 每次请求 completion 之前都要先要一个 challenge；
//   - 算法名固定 DeepSeekHashV1（Keccak 变体，23 轮），要求
//     hash(salt + "_" + expire_at + "_" + decimal(i)) 的前 32 字节等于 challenge；
//   - 结果塞进 x-ds-pow-response 头：base64(JSON{algorithm,challenge,salt,signature,answer,target_path})；
//   - difficulty 是搜索上限（≤5,000,000），求解超时要放弃。
//
// 其余「像反爬」的东西其实只是网页客户端指纹：x-client-platform / x-client-version /
// x-client-bundle-id / x-client-locale 四个头，必须和网页版一致。
//
// 会话组织：DeepSeek 按 conversation 组织，一轮对话 = 一个 chat_session_id +
// parent_message_id（上一轮的 assistant message_id）。本包不做历史存储，只透传这两个 id。
package deepseekweb

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 协议常量。全部对应 Java 侧 DeepSeekWeb 里硬编码的那一套。
const (
	// ProviderID 是供应商 id。
	ProviderID = "deepseek_web"

	// Origin 网页版根地址。
	Origin = "https://chat.deepseek.com"
	// PathCompletion 推理/对话端点（SSE）。
	PathCompletion = "/api/v0/chat/completion"
	// PathSessionCreate 建会话。
	PathSessionCreate = "/api/v0/chat_session/create"
	// PathSessionDelete 删会话。
	PathSessionDelete = "/api/v0/chat_session/delete"
	// PathCurrentUser 校验 token 用。
	PathCurrentUser = "/api/v0/users/current"
	// PathPowChallenge 领 PoW 挑战。
	PathPowChallenge = "/api/v0/chat/create_pow_challenge"
	// PathResumeStream 首轮空回复时重放。
	PathResumeStream = "/api/v0/chat/resume_stream"
	// PathContinue 上游 INCOMPLETE 时续写。
	PathContinue = "/api/v0/chat/continue"
)

// 网页客户端指纹，四个值都必须原样带上。
const (
	// ClientPlatform 固定 web。
	ClientPlatform = "web"
	// ClientVersion 网页版前端版本号。
	ClientVersion = "2.4.0"
	// ClientBundleID 网页版的包标识。
	ClientBundleID = "com.deepseek.chat"
	// ClientLocale 报文语言。
	ClientLocale = "en_US"
)

// 模式别名。这是网页版的「模式」，不是厂商的基础模型 id。
const (
	// ModelChat 普通对话（model_type=default）。
	ModelChat = "deepseek-chat"
	// ModelReasoner 深度思考（model_type=expert，thinking_enabled=true）。
	ModelReasoner = "deepseek-reasoner"
	// ModelTypeDefault 普通模式。
	ModelTypeDefault = "default"
	// ModelTypeExpert 深度思考模式。
	ModelTypeExpert = "expert"
)

// Models 是网页版支持的模型别名白名单。
var Models = []string{ModelChat, ModelReasoner}

// 上限常量。
const (
	// MaxDifficulty PoW 的 difficulty 上限，超过直接判为异常挑战。
	MaxDifficulty = 5000000
	// powRounds PoW 用的 Keccak 轮数（比标准 24 轮少一轮，是上游魔改）。
	powRounds = 23
	// keccakRate 吸收率（字节）：136 = (1600-2*256)/8。
	keccakRate = 136
	// maxBody 单次响应体上限。
	maxBody = 1 << 20
	// maxPowTimeout PoW 求解超时上限。
	maxPowTimeout = 120 * time.Second
	// maxContinues INCOMPLETE 续写次数上限。
	maxContinues = 20
)

// 头名。
const (
	// HeaderPow 是 PoW 答案头。
	HeaderPow = "x-ds-pow-response"
)

// HTTPClient 是默认客户端。completion 是长连接流式请求，超时交给 ctx 控制。
var HTTPClient = &http.Client{}

// ErrFinished 等哨兵错误。
var (
	// ErrPowTimeout PoW 求解超时。
	ErrPowTimeout = errors.New("deepseekweb: PoW 求解超时或被取消")
	// ErrPowUnsolved 在 difficulty 范围内没找到解。
	ErrPowUnsolved = errors.New("deepseekweb: 在 challenge difficulty 范围内未解出 PoW")
	// ErrStreamDone 读流结束（内部用）。
	errStreamDone = errors.New("deepseekweb: stream done")
)

// tokenPattern token 必须全部是可打印 ASCII。
var tokenPattern = regexp.MustCompile(`^[\x21-\x7e]+$`)

// fragmentPathPattern 匹配 response/fragments/<index>[/content]。
var fragmentPathPattern = regexp.MustCompile(`^response/fragments/(-?[0-9]+)(/content)?$`)

// Account 是一个 DeepSeek 网页版账号。token 就是浏览器里那条 Bearer。
type Account struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Type  string `json:"type,omitempty"`
	Email string `json:"email,omitempty"`
	// Token 网页版 access token（localStorage 里的 userToken）。
	Token string `json:"token,omitempty"`
	// Access 与 Token 同值，兼容只写 access 的配置。
	Access string `json:"access,omitempty"`
	// UserID 网页版用户 id。
	UserID    string `json:"uid,omitempty"`
	AddedAt   int64  `json:"addedAt,omitempty"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`
	Enabled   bool   `json:"enabled,omitempty"`
}

// WebToken 返回网页版 token（Token 优先，回退 Access）。
func (a *Account) WebToken() string {
	if a == nil {
		return ""
	}
	if a.Token != "" {
		return a.Token
	}
	return a.Access
}

// Headers 返回网页版接口所需的完整头集合。
//
// 注意 referer 是 Origin + "/a/chat/"，缺了它或 x-client-* 之一，上游会按「第三方调用」拒掉。
func Headers(acc *Account) map[string]string {
	h := map[string]string{
		"accept":             "*/*",
		"content-type":       "application/json",
		"authorization":      "Bearer " + acc.WebToken(),
		"origin":             Origin,
		"referer":            Origin + "/a/chat/",
		"x-client-platform":  ClientPlatform,
		"x-client-version":   ClientVersion,
		"x-client-bundle-id": ClientBundleID,
		"x-client-locale":    ClientLocale,
	}
	return h
}

// UpstreamError 是上游失败的结构化错误。错误详情（响应体）单独保留，方便调用方脱敏后上报。
type UpstreamError struct {
	Message        string
	UpstreamStatus int
	GatewayStatus  int
	Body           string
	BodyTruncated  bool
}

// Error 实现 error。
func (e *UpstreamError) Error() string { return e.Message }

// ---------------------------------------------------------------------------
// 传输与信封拆解
// ---------------------------------------------------------------------------

// Client 是网页版协议客户端。
type Client struct {
	// Token 网页版 Bearer token。
	Token string
	// BaseURL 默认 Origin，测试时可指向本地桩。
	BaseURL string
	// HTTP 默认 HTTPClient。
	HTTP *http.Client
}

// NewClient 构造客户端，并做一次 token 形态校验。
func NewClient(token string) (*Client, error) {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bearer "))
	if !tokenPattern.MatchString(token) {
		return nil, errors.New("deepseekweb: token 非法（必须是可打印 ASCII）")
	}
	return &Client{Token: token, BaseURL: Origin, HTTP: HTTPClient}, nil
}

// Call 发一次请求并返回解析后的 JSON 信封。
func (c *Client) Call(ctx context.Context, method, path string, body any, extra map[string]string) (map[string]any, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(raw)
	}
	base := c.BaseURL
	if base == "" {
		base = Origin
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, payload)
	if err != nil {
		return nil, err
	}
	for k, v := range Headers(&Account{Token: c.Token}) {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if client == nil {
		client = HTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited, truncated := readLimited(resp.Body, maxBody)
		mapped := 502
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			mapped = http.StatusUnauthorized
		case http.StatusTooManyRequests:
			mapped = http.StatusTooManyRequests
		}
		return nil, &UpstreamError{
			Message:        fmt.Sprintf("deepseekweb: HTTP %d", resp.StatusCode),
			UpstreamStatus: resp.StatusCode,
			GatewayStatus:  mapped,
			Body:           string(limited),
			BodyTruncated:  truncated,
		}
	}
	raw, truncated := readLimited(resp.Body, maxBody)
	if truncated {
		return nil, errors.New("deepseekweb: 响应体超过 1MB")
	}
	return decodeObject(raw)
}

// readLimited 按上限读响应体，返回内容与是否被截断。
func readLimited(r io.Reader, limit int) ([]byte, bool) {
	var buf bytes.Buffer
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			take := limit - buf.Len()
			if take > n {
				take = n
			}
			if take > 0 {
				buf.Write(chunk[:take])
			}
			if take < n {
				return buf.Bytes(), true
			}
		}
		if err != nil {
			return buf.Bytes(), false
		}
	}
}

// unwrap 拆 {code,msg,data:{biz_code,biz_data}} 信封，返回 biz_data。
//
// allow22 允许 biz_code=22（completion 的正常「继续」状态码）。
func unwrap(root map[string]any, op string, allow22 bool) (map[string]any, error) {
	if root == nil {
		return nil, nil
	}
	if err, has := root["error"]; has && err != nil {
		return nil, fmt.Errorf("deepseekweb: %s 失败（error 字段）", op)
	}
	if code := pickString(root, "code"); code != "" && code != "0" && !(allow22 && code == "22") {
		return nil, fmt.Errorf("deepseekweb: %s 被拒（code=%s）", op, code)
	}
	data, _ := root["data"].(map[string]any)
	bizCode := pickString(root, "code")
	if data != nil {
		if v := pickString(data, "biz_code"); v != "" {
			bizCode = v
		}
	}
	if bizCode != "" && bizCode != "0" && !(allow22 && bizCode == "22") {
		return nil, fmt.Errorf("deepseekweb: %s 被拒（biz_code=%s）", op, bizCode)
	}
	if data == nil {
		return nil, nil
	}
	bizData, _ := data["biz_data"].(map[string]any)
	return bizData, nil
}

// ValidateToken 用只读的 /users/current 校验 token，成功返回用户对象。
func (c *Client) ValidateToken(ctx context.Context) (map[string]any, error) {
	root, err := c.Call(ctx, http.MethodGet, PathCurrentUser, nil, nil)
	if err != nil {
		return nil, err
	}
	biz, err := unwrap(root, "token 校验", false)
	if err != nil {
		return nil, err
	}
	if biz == nil {
		return nil, errors.New("deepseekweb: token 校验未返回用户")
	}
	user, _ := biz["user"].(map[string]any)
	if user == nil {
		user = biz
	}
	for _, key := range []string{"id", "user_id", "mobile_number", "mobile", "email"} {
		if v, ok := user[key].(string); ok && strings.TrimSpace(v) != "" {
			return user, nil
		}
		if (key == "id" || key == "user_id") && pickInt(user, key) > 0 {
			return user, nil
		}
	}
	return nil, errors.New("deepseekweb: token 校验未返回用户身份")
}

// CreateSession 建一个会话，返回 chat_session_id。
func (c *Client) CreateSession(ctx context.Context) (string, error) {
	root, err := c.Call(ctx, http.MethodPost, PathSessionCreate, map[string]any{}, nil)
	if err != nil {
		return "", err
	}
	biz, err := unwrap(root, "建会话", false)
	if err != nil {
		return "", err
	}
	id := ""
	if biz != nil {
		if session, ok := biz["chat_session"].(map[string]any); ok {
			id = pickString(session, "id")
		}
		if id == "" {
			id = pickString(biz, "id")
		}
	}
	if id == "" {
		return "", errors.New("deepseekweb: 会话 id 缺失")
	}
	return id, nil
}

// DeleteSession 删会话。调用方自己决定何时删（网页版用完不删会一直挂在历史里）。
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	root, err := c.Call(ctx, http.MethodPost, PathSessionDelete, map[string]any{"chat_session_id": id}, nil)
	if err != nil {
		return err
	}
	_, err = unwrap(root, "删会话", false)
	return err
}

// ---------------------------------------------------------------------------
// 请求体：Chat Completions → 网页版 completion
// ---------------------------------------------------------------------------

// CompletionRequest 是 POST /api/v0/chat/completion 的请求体。
// 字段顺序与 Java 侧保持一致（上游对顺序不敏感，但便于逐字节比对）。
type CompletionRequest struct {
	// Action 固定 null。
	Action any `json:"action"`
	// ChatSessionID 会话 id（conversation）。
	ChatSessionID string `json:"chat_session_id"`
	// ParentMessageID 上一轮 assistant 的 message_id；首轮为 null。这就是「会话链」。
	ParentMessageID *int64 `json:"parent_message_id"`
	// ModelType default（普通）/ expert（深度思考）。
	ModelType string `json:"model_type"`
	// Prompt 摊平后的整段对话文本（网页版没有 messages 数组）。
	Prompt string `json:"prompt"`
	// RefFileIDs 附件 id，文本模式固定空数组。
	RefFileIDs []string `json:"ref_file_ids"`
	// ThinkingEnabled 是否开深度思考。
	ThinkingEnabled bool `json:"thinking_enabled"`
	// SearchEnabled 是否允许联网搜索。
	SearchEnabled bool `json:"search_enabled"`
	// Preempt 是否抢占正在生成的回复。
	Preempt bool `json:"preempt"`
}

// 工具调用提示词（网页版没有 tools 字段，只能在 prompt 里教模型输出约定标记）。
const toolPreamble = "[Tool calling]\n" +
	"You can call the tools listed at the end of this block. To call one, end your reply with a line of exactly this shape:\n" +
	`<tool_call>{"name":"tool name","arguments":{}}</tool_call>` + "\n" +
	"- \"arguments\" carries the tool's parameters as JSON, following its schema.\n" +
	"You may output several <tool_call> blocks in one reply to call tools in parallel.\n" +
	"Do not wrap a call in code fences, and write nothing after the calls: their results arrive with the next turn.\n" +
	"Use exactly this markup — never the DSML/内部工具 format, never any other tag style.\n" +
	"When no tool fits, answer normally and call nothing.\n" +
	"The tools:"

// BuildChatRequest 把 OpenAI 风格的 Chat Completions 请求体（JSON）转换成网页版
// completion 请求体（JSON）。
//
// sessionID / parentID 由调用方维护：新对话传空 sessionID（Run 会自动建会话），
// 续聊传上次 TurnResult 返回的 sessionID 与 messageID。
func BuildChatRequest(chatBody []byte, sessionID string, parentID *int64) ([]byte, error) {
	req, err := Prepare(chatBody, sessionID, parentID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(req)
}

// Prepare 校验并构造请求体（对应 Java 的 DeepSeekWeb.prepare）。
//
// 会明确拒绝而不是静默丢弃：functions/function_call、非文本 content、非法 tool_calls。
func Prepare(chatBody []byte, sessionID string, parentID *int64) (*CompletionRequest, error) {
	chat, err := decodeObject(chatBody)
	if err != nil {
		return nil, fmt.Errorf("deepseekweb: Chat 请求体无法解析: %w", err)
	}
	model := pickString(chat, "model")
	if !containsString(Models, model) {
		return nil, fmt.Errorf("deepseekweb: 模型 %q 不支持（只接受 deepseek-chat / deepseek-reasoner）", model)
	}
	if _, has := chat["functions"]; has {
		return nil, errors.New("deepseekweb: 不支持 functions")
	}
	if _, has := chat["function_call"]; has {
		return nil, errors.New("deepseekweb: 不支持 function_call")
	}
	messages, ok := chat["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("deepseekweb: messages 必填")
	}
	for i, raw := range messages {
		m, isObj := raw.(map[string]any)
		if !isObj {
			return nil, fmt.Errorf("deepseekweb: messages[%d] 不是对象", i)
		}
		role := pickString(m, "role")
		switch role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("deepseekweb: 不支持的 role %q", role)
		}
		if _, has := m["function_call"]; has {
			return nil, fmt.Errorf("deepseekweb: messages[%d].function_call 不支持", i)
		}
		switch v := m["content"].(type) {
		case string:
			// 纯文本，直接用
		case []any:
			for j, pRaw := range v {
				part, isObj := pRaw.(map[string]any)
				if !isObj || pickString(part, "type") != "text" {
					return nil, fmt.Errorf("deepseekweb: messages[%d].content[%d] 只支持 text", i, j)
				}
				if _, isStr := part["text"].(string); !isStr {
					return nil, fmt.Errorf("deepseekweb: messages[%d].content[%d].text 必须是字符串", i, j)
				}
			}
		case nil:
			// 只有 assistant + tool_calls 才允许正文为空（网页版靠 tool_call 标记续接）
			if calls, ok := m["tool_calls"].([]any); !ok || role != "assistant" || len(calls) == 0 {
				return nil, fmt.Errorf("deepseekweb: messages[%d].content 非法", i)
			}
		default:
			return nil, fmt.Errorf("deepseekweb: messages[%d].content 类型非法", i)
		}
		if callsRaw, has := m["tool_calls"]; has {
			calls, isArr := callsRaw.([]any)
			if role != "assistant" || !isArr {
				return nil, fmt.Errorf("deepseekweb: messages[%d].tool_calls 非法", i)
			}
			for j, cRaw := range calls {
				c, isObj := cRaw.(map[string]any)
				if !isObj {
					return nil, fmt.Errorf("deepseekweb: messages[%d].tool_calls[%d] 非法", i, j)
				}
				fn, isObj := c["function"].(map[string]any)
				if pickString(c, "type") != "function" || pickString(c, "id") == "" || !isObj {
					return nil, fmt.Errorf("deepseekweb: messages[%d].tool_calls[%d] 非法", i, j)
				}
				if pickString(fn, "name") == "" {
					return nil, fmt.Errorf("deepseekweb: messages[%d].tool_calls[%d].function.name 缺失", i, j)
				}
				args, isStr := fn["arguments"].(string)
				if !isStr {
					return nil, fmt.Errorf("deepseekweb: messages[%d].tool_calls[%d].function.arguments 必须是字符串", i, j)
				}
				if _, err := decodeObject([]byte(args)); err != nil {
					return nil, fmt.Errorf("deepseekweb: messages[%d].tool_calls[%d].function.arguments 不是合法 JSON 对象", i, j)
				}
			}
		}
		if role == "tool" && pickString(m, "tool_call_id") == "" {
			return nil, fmt.Errorf("deepseekweb: messages[%d].tool_call_id 缺失", i)
		}
	}

	tools, toolEnabled, err := analyzeTools(chat)
	if err != nil {
		return nil, err
	}
	prompt := buildPrompt(messages, tools, toolEnabled)

	// 模型别名决定模式：reasoner 走 expert + 深度思考，chat 走 default。
	modelType, thinking := ModelTypeDefault, false
	if model == ModelReasoner {
		modelType, thinking = ModelTypeExpert, true
	}
	return &CompletionRequest{
		Action:          nil,
		ChatSessionID:   sessionID,
		ParentMessageID: parentID,
		ModelType:       modelType,
		Prompt:          prompt,
		RefFileIDs:      []string{},
		ThinkingEnabled: thinking,
		SearchEnabled:   pickBool(chat, "search_enabled", true),
		Preempt:         false,
	}, nil
}

// analyzeTools 解析 tools 白名单，返回工具列表、名字集合、是否启用。
//
// 规则与 Java 一致：tool_choice 只接受 auto/none（具名调用网页版表达不了），
// 工具必须是 function 类型且名字唯一。
func analyzeTools(chat map[string]any) ([]any, bool, error) {
	choice := "auto"
	if raw, has := chat["tool_choice"]; has && raw != nil {
		s, isStr := raw.(string)
		if !isStr || (s != "auto" && s != "none") {
			// 具名调用网页版表达不了，明确拒绝
			return nil, false, errors.New("deepseekweb: tool_choice 只支持 auto/none")
		}
		choice = s
	}
	if raw, has := chat["tools"]; has && raw != nil {
		if _, isArr := raw.([]any); !isArr {
			return nil, false, errors.New("deepseekweb: tools 必须是数组")
		}
	}
	tools, _ := chat["tools"].([]any)
	names := map[string]bool{}
	kept := make([]any, 0, len(tools))
	for i, raw := range tools {
		t, isObj := raw.(map[string]any)
		if !isObj || pickString(t, "type") != "function" {
			return nil, false, fmt.Errorf("deepseekweb: tools[%d] 只支持 function 类型", i)
		}
		fn, isObj := t["function"].(map[string]any)
		if !isObj {
			return nil, false, fmt.Errorf("deepseekweb: tools[%d].function 缺失", i)
		}
		name := strings.TrimSpace(pickString(fn, "name"))
		if name == "" {
			return nil, false, fmt.Errorf("deepseekweb: tools[%d] 名称为空", i)
		}
		if names[name] {
			return nil, false, fmt.Errorf("deepseekweb: tools[%d] 名称重复（%s）", i, name)
		}
		names[name] = true
		kept = append(kept, fn)
	}
	return kept, choice != "none" && len(kept) > 0, nil
}

// buildPrompt 把 messages 摊平成一段纯文本（对应 Java 的 DeepSeekToolBridge.Prepared.buildPrompt）。
//
// 约定：System/Developer 原文顶格；assistant 前缀 "Assistant: "；工具结果写成
// "Tool result (name, id): ..."；user 只有从第二条起才加 "User: " 前缀。
func buildPrompt(messages []any, tools []any, toolEnabled bool) string {
	var parts []string
	if toolEnabled {
		var sb strings.Builder
		sb.WriteString(toolPreamble)
		for _, raw := range tools {
			fn, _ := raw.(map[string]any)
			row := map[string]any{
				"name":        pickString(fn, "name"),
				"description": pickString(fn, "description"),
				"parameters":  fn["parameters"],
			}
			if row["parameters"] == nil {
				row["parameters"] = map[string]any{}
			}
			b, err := json.Marshal(row)
			if err != nil {
				continue
			}
			sb.WriteString("\n")
			sb.Write(b)
		}
		parts = append(parts, sb.String())
	}

	// 历史里的 tool_call id → 工具名，工具结果要能标出对应哪个工具。
	ids := map[string]string{}
	for _, raw := range messages {
		m, _ := raw.(map[string]any)
		calls, _ := m["tool_calls"].([]any)
		for _, cRaw := range calls {
			c, _ := cRaw.(map[string]any)
			fn, _ := c["function"].(map[string]any)
			if fn == nil {
				continue
			}
			ids[pickString(c, "id")] = pickString(fn, "name")
		}
	}

	spoke := false
	for _, raw := range messages {
		m, _ := raw.(map[string]any)
		role := pickString(m, "role")
		text := messageText(m["content"])
		switch role {
		case "system", "developer":
			if text != "" {
				parts = append(parts, text)
			}
		case "assistant":
			var sb strings.Builder
			sb.WriteString(text)
			calls, _ := m["tool_calls"].([]any)
			for _, cRaw := range calls {
				c, _ := cRaw.(map[string]any)
				fn, _ := c["function"].(map[string]any)
				name := pickString(fn, "name")
				if name == "" {
					continue
				}
				args := "{}"
				switch v := fn["arguments"].(type) {
				case string:
					if strings.TrimSpace(v) != "" {
						args = v
					}
				case map[string]any:
					if b, err := json.Marshal(v); err == nil {
						args = string(b)
					}
				}
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(`<tool_call>{`)
				if id := pickString(c, "id"); id != "" {
					sb.WriteString(`"id":` + quoteJSON(id) + ",")
				}
				sb.WriteString(`"name":` + quoteJSON(name) + `,"arguments":` + args + `}</tool_call>`)
			}
			if sb.Len() > 0 {
				parts = append(parts, "Assistant: "+sb.String())
			}
		case "tool":
			if text == "" {
				continue
			}
			callID := pickString(m, "tool_call_id")
			name := ids[callID]
			label := ""
			switch {
			case name == "" && callID == "":
				label = ""
			case name == "":
				label = " (id=" + callID + ")"
			case callID == "":
				label = " (" + name + ")"
			default:
				label = " (" + name + ", id=" + callID + ")"
			}
			parts = append(parts, "Tool result"+label+": "+text)
		default:
			if text == "" {
				continue
			}
			if spoke {
				parts = append(parts, "User: "+text)
			} else {
				parts = append(parts, text)
				spoke = true
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// messageText 取出消息正文（字符串或 text parts 数组）。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, raw := range v {
			switch p := raw.(type) {
			case string:
				sb.WriteString(p)
			case map[string]any:
				sb.WriteString(pickString(p, "text"))
			}
		}
		return sb.String()
	}
	return ""
}

// ---------------------------------------------------------------------------
// PoW（DeepSeekHashV1）
// ---------------------------------------------------------------------------

// Challenge 是 /create_pow_challenge 返回的挑战。
type Challenge struct {
	// Algorithm 固定 DeepSeekHashV1。
	Algorithm string `json:"algorithm"`
	// Challenge 目标摘要的十六进制（64 字符 = 32 字节）。
	Challenge string `json:"challenge"`
	// Salt 前缀盐。
	Salt string `json:"salt"`
	// Signature 原样回传。
	Signature string `json:"signature"`
	// Difficulty 搜索上限，也是难度。
	Difficulty int64 `json:"difficulty"`
	// ExpireAt 参与哈希的前缀组成部分。
	ExpireAt int64 `json:"expire_at"`
}

// rotc 与 rc 是 Keccak-f[1600] 的旋转量表与轮常量（与 Java 侧 DeepSeekWeb 完全一致）。
var rotc = [25]int{0, 1, 62, 28, 27, 36, 44, 6, 55, 20, 3, 10, 43, 25, 39, 41, 45, 15, 21, 8, 18, 2, 61, 56, 14}

var rc = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
	0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
	0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// keccakF 执行 rounds 轮 Keccak-f 置换。
//
// 注意上游的「rounds」是倒数：rounds=23 表示跳过 RC[0] 那一轮，只跑后 23 轮。
func keccakF(s *[25]uint64, rounds int) {
	var c, d, b [25]uint64
	for r := 24 - rounds; r < 24; r++ {
		for x := 0; x < 5; x++ {
			c[x] = s[x] ^ s[x+5] ^ s[x+10] ^ s[x+15] ^ s[x+20]
		}
		for x := 0; x < 5; x++ {
			d[x] = c[(x+4)%5] ^ bits.RotateLeft64(c[(x+1)%5], 1)
		}
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				i := x + 5*y
				s[i] ^= d[x]
				b[y+5*((2*x+3*y)%5)] = bits.RotateLeft64(s[i], rotc[i])
			}
		}
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				s[x+5*y] = b[x+5*y] ^ ((^b[(x+1)%5+5*y]) & b[(x+2)%5+5*y])
			}
		}
		s[0] ^= rc[r]
	}
}

// Hash 计算 Keccak 变体摘要（rounds=powRounds 即上游的 DeepSeekHashV1）。
func Hash(input []byte, rounds int) [32]byte {
	var s [25]uint64
	pos := 0
	for _, v := range input {
		s[pos/8] ^= uint64(v) << uint((pos%8)*8)
		pos++
		if pos == keccakRate {
			keccakF(&s, rounds)
			pos = 0
		}
	}
	// 0x06 域分隔 + 末尾 0x80（这里落在 lane16 的最高位，与上游实现一致）
	s[pos/8] ^= 6 << uint((pos%8)*8)
	s[16] ^= 1 << 63
	keccakF(&s, rounds)

	var out [32]byte
	for i := 0; i < 32; i++ {
		out[i] = byte(s[i/8] >> uint((i%8)*8))
	}
	return out
}

// ValidateChallenge 做与上游一致的挑战合法性检查，避免被喂超大 difficulty 白烧 CPU。
func ValidateChallenge(c Challenge, timeout time.Duration) ([]byte, error) {
	if c.Algorithm != "DeepSeekHashV1" {
		return nil, fmt.Errorf("deepseekweb: 未知 PoW 算法 %q", c.Algorithm)
	}
	if len(c.Challenge) != 64 {
		return nil, errors.New("deepseekweb: PoW challenge 长度非法")
	}
	want, err := hex.DecodeString(c.Challenge)
	if err != nil {
		return nil, errors.New("deepseekweb: PoW challenge 不是十六进制")
	}
	if c.Salt == "" || len(c.Salt) > 1024 {
		return nil, errors.New("deepseekweb: PoW salt 长度非法")
	}
	if c.Difficulty < 1 || c.Difficulty > MaxDifficulty {
		return nil, fmt.Errorf("deepseekweb: PoW difficulty 超出范围（%d）", c.Difficulty)
	}
	if c.ExpireAt <= 0 {
		return nil, errors.New("deepseekweb: PoW expire_at 非法")
	}
	if timeout <= 0 || timeout > maxPowTimeout {
		return nil, errors.New("deepseekweb: PoW 超时参数非法")
	}
	return want, nil
}

// SolvePoW 暴力搜索 answer：hash(salt + "_" + expire_at + "_" + decimal(i)) 前 32 字节 == challenge。
//
// 每 64 次检查一次超时与 ctx，避免长时间卡住无法取消。
func SolvePoW(ctx context.Context, c Challenge, timeout time.Duration) (int, error) {
	want, err := ValidateChallenge(c, timeout)
	if err != nil {
		return 0, err
	}
	deadline := time.Now().Add(timeout)
	prefix := []byte(c.Salt + "_" + strconv.FormatInt(c.ExpireAt, 10) + "_")
	buf := make([]byte, len(prefix), len(prefix)+24)
	copy(buf, prefix)

	for i := int64(0); i < c.Difficulty; i++ {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, fmt.Errorf("%w: %v", ErrPowTimeout, err)
			}
			if !time.Now().Before(deadline) {
				return 0, ErrPowTimeout
			}
		}
		candidate := strconv.AppendInt(buf, i, 10)
		sum := Hash(candidate, powRounds)
		if subtle.ConstantTimeCompare(sum[:], want) == 1 {
			return int(i), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrPowTimeout, err)
	}
	return 0, ErrPowUnsolved
}

// PowHeader 把解出的答案组装成 x-ds-pow-response 的值（标准 base64，带 padding）。
func PowHeader(c Challenge, answer int) (string, error) {
	if c.Algorithm == "" || c.Challenge == "" || c.Salt == "" {
		return "", errors.New("deepseekweb: PoW 挑战缺字段，无法组装头")
	}
	payload := struct {
		Algorithm  string `json:"algorithm"`
		Challenge  string `json:"challenge"`
		Salt       string `json:"salt"`
		Signature  string `json:"signature"`
		Answer     int    `json:"answer"`
		TargetPath string `json:"target_path"`
	}{
		Algorithm: c.Algorithm, Challenge: c.Challenge, Salt: c.Salt,
		Signature: c.Signature, Answer: answer, TargetPath: PathCompletion,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// FetchChallenge 领取一次 PoW 挑战。
func (c *Client) FetchChallenge(ctx context.Context) (Challenge, error) {
	root, err := c.Call(ctx, http.MethodPost, PathPowChallenge,
		map[string]any{"target_path": PathCompletion}, nil)
	if err != nil {
		return Challenge{}, err
	}
	biz, err := unwrap(root, "领 PoW 挑战", false)
	if err != nil {
		return Challenge{}, err
	}
	var out Challenge
	if biz != nil {
		if raw, ok := biz["challenge"]; ok {
			obj, isObj := raw.(map[string]any)
			if !isObj {
				return Challenge{}, errors.New("deepseekweb: PoW 挑战结构非法")
			}
			out = Challenge{
				Algorithm:  pickString(obj, "algorithm"),
				Challenge:  pickString(obj, "challenge"),
				Salt:       pickString(obj, "salt"),
				Signature:  pickString(obj, "signature"),
				Difficulty: pickInt(obj, "difficulty"),
				ExpireAt:   pickInt(obj, "expire_at"),
			}
			if out.Algorithm == "" {
				return Challenge{}, errors.New("deepseekweb: PoW 挑战缺算法名")
			}
			return out, nil
		}
	}
	return Challenge{}, errors.New("deepseekweb: PoW 挑战缺失")
}

// ---------------------------------------------------------------------------
// SSE 增量解析（patch 语义）
// ---------------------------------------------------------------------------

// StreamState 累积上游 SSE patch 并回调净增量。
//
// 上游不是简单吐 delta，而是下发「打补丁」帧：
//
//	{"p":"response/status","v":"FINISHED"}                       状态
//	{"p":"response/message_id","v":123}                          消息 id（下一轮的 parent）
//	{"p":"response/fragments","v":[...],"o":"SET"}               快照
//	{"p":"response/fragments/0/content","v":"…","o":"APPEND"}    追加
//	{"p":"…","o":"BATCH","v":[...]}                              批量
//
// fragment 的 type 决定进正文还是进思考链：RESPONSE/TEMPLATE_RESPONSE 是正文，
// THINK/THINKING/REASONING/SEARCH/TOOL_* 都进思考链。
type StreamState struct {
	// Text 正文。
	Text strings.Builder
	// Reasoning 思考链/搜索摘要。
	Reasoning strings.Builder
	// MessageID 本轮 assistant message 的 id，下一轮当 parent_message_id 用。
	MessageID int64
	// HasMessageID 是否拿到过 message id。
	HasMessageID bool
	// Status 上游状态：FINISHED / INCOMPLETE / WIP。
	Status string
	// Usage 累计 token 用量。
	Usage int64
	// Closed 上游发了 close 事件。
	Closed bool

	// OnText 正文增量回调。
	OnText func(string) error
	// OnReasoning 思考链增量回调。
	OnReasoning func(string) error

	kind     string
	contents []string
	kinds    []string
}

// NewStreamState 构造一个新的流状态。
func NewStreamState(onText, onReasoning func(string) error) *StreamState {
	return &StreamState{OnText: onText, OnReasoning: onReasoning}
}

// checkFrameError 检查帧里的错误标记（error 字段、非 0 的 code/biz_code）。
func checkFrameError(frame map[string]any) error {
	if frame == nil {
		return nil
	}
	if v, has := frame["error"]; has && v != nil {
		return errors.New("deepseekweb: 流内返回 error")
	}
	if p := pickString(frame, "p"); p == "error" || p == "response/error" || p == "data/error" {
		if v, has := frame["v"]; has && v != nil {
			return fmt.Errorf("deepseekweb: 流内错误补丁 %s", p)
		}
	}
	for _, key := range []string{"code", "biz_code"} {
		if v := pickString(frame, key); v != "" && v != "0" {
			return fmt.Errorf("deepseekweb: 流被拒绝（%s=%s）", key, v)
		}
	}
	if data, ok := frame["data"].(map[string]any); ok {
		if v, has := data["error"]; has && v != nil {
			return errors.New("deepseekweb: 流被拒绝")
		}
		if v := pickString(data, "biz_code"); v != "" && v != "0" {
			return fmt.Errorf("deepseekweb: 流被拒绝（biz_code=%s）", v)
		}
	}
	return nil
}

// appendDelta 按 fragment 类型把增量分流到正文或思考链。
func (s *StreamState) appendDelta(kind, text string) error {
	if text == "" {
		return nil
	}
	t := strings.ToUpper(kind)
	thinking := t == "THINK" || t == "THINKING" || t == "REASONING" || t == "SEARCH" || strings.HasPrefix(t, "TOOL_")
	switch {
	case thinking:
		s.Reasoning.WriteString(text)
		if s.OnReasoning != nil {
			return s.OnReasoning(text)
		}
	case t == "" || t == "RESPONSE" || t == "TEMPLATE_RESPONSE":
		s.Text.WriteString(text)
		if s.OnText != nil {
			return s.OnText(text)
		}
	default:
		return fmt.Errorf("deepseekweb: 不支持的 fragment 类型 %q", t)
	}
	return nil
}

// writeFragment 写某个 fragment。snapshot=true 表示 v 是全文（要自己算增量）。
func (s *StreamState) writeFragment(index int, value string, snapshot bool) error {
	if index < 0 || index >= len(s.contents) {
		return errors.New("deepseekweb: 未知 fragment 下标")
	}
	old := s.contents[index]
	delta := value
	if snapshot {
		if !strings.HasPrefix(value, old) {
			return errors.New("deepseekweb: 上游改写了已输出的文本")
		}
		delta = value[len(old):]
		s.contents[index] = value
	} else {
		s.contents[index] = old + value
	}
	return s.appendDelta(s.kinds[index], delta)
}

// fragments 处理 fragment 数组。snapshot=true 时按下标对齐为快照。
func (s *StreamState) fragments(list []any, snapshot bool) error {
	for i, raw := range list {
		f, isObj := raw.(map[string]any)
		if !isObj {
			return errors.New("deepseekweb: fragment 结构非法")
		}
		index := len(s.contents)
		if snapshot {
			index = i
		}
		kind := pickString(f, "type")
		if kind == "" {
			kind = s.kind
		}
		s.kind = kind
		for len(s.contents) <= index {
			s.contents = append(s.contents, "")
			s.kinds = append(s.kinds, kind)
		}
		s.kinds[index] = kind
		if v, has := f["content"]; has && v != nil {
			text, isStr := v.(string)
			if !isStr {
				return errors.New("deepseekweb: fragment content 非法")
			}
			if err := s.writeFragment(index, text, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// tree 处理「整棵树」形态的帧（v 是对象而不是补丁）。
func (s *StreamState) tree(v map[string]any) error {
	response, _ := v["response"].(map[string]any)
	if response == nil {
		if inner, ok := v["v"].(map[string]any); ok {
			response, _ = inner["response"].(map[string]any)
		}
	}
	if response != nil {
		if raw, has := response["message_id"]; has && raw != nil {
			if id, err := asInt(raw); err == nil {
				s.MessageID = id
				s.HasMessageID = true
			}
		}
		if _, has := response["status"]; has {
			s.Status = pickString(response, "status")
		}
		if raw, has := response["accumulated_token_usage"]; has && raw != nil {
			if u, err := asInt(raw); err == nil {
				s.Usage = u
			}
		}
		list, _ := response["fragments"].([]any)
		if err := s.fragments(list, true); err != nil {
			return err
		}
	}
	if _, has := v["status"]; has {
		s.Status = pickString(v, "status")
	}
	if raw, has := v["message_id"]; has && raw != nil {
		if id, err := asInt(raw); err == nil {
			s.MessageID = id
			s.HasMessageID = true
		}
	}
	return nil
}

// Apply 处理一帧 patch。
func (s *StreamState) Apply(frame map[string]any) error {
	if err := checkFrameError(frame); err != nil {
		return err
	}
	if nested, ok := frame["v"].(map[string]any); ok {
		if err := checkFrameError(nested); err != nil {
			return err
		}
	}
	p := pickString(frame, "p")
	v := frame["v"]
	op := pickString(frame, "o")

	// BATCH：v 是数组，每项都是补丁；子路径要拼上父路径。
	if op == "BATCH" {
		list, isArr := v.([]any)
		if !isArr {
			return errors.New("deepseekweb: 批量补丁结构非法")
		}
		for _, raw := range list {
			child, isObj := raw.(map[string]any)
			if !isObj {
				return errors.New("deepseekweb: 批量补丁项非法")
			}
			childPath := pickString(child, "p")
			if p != "" && childPath != "" && !strings.HasPrefix(childPath, p+"/") {
				merged := cloneMap(child)
				merged["p"] = p + "/" + childPath
				if err := s.Apply(merged); err != nil {
					return err
				}
				continue
			}
			if err := s.Apply(child); err != nil {
				return err
			}
		}
		return nil
	}

	if p != "" {
		switch {
		case p == "response/status":
			if text, ok := v.(string); ok {
				s.Status = text
			}
		case p == "response/message_id":
			if id, err := asInt(v); err == nil {
				s.MessageID = id
				s.HasMessageID = true
			}
		case strings.Contains(p, "accumulated_token_usage"):
			if u, err := asInt(v); err == nil {
				s.Usage = u
			}
		case p == "response/fragments":
			if list, ok := v.([]any); ok {
				return s.fragments(list, op == "SET")
			}
		case fragmentPathPattern.MatchString(p):
			text, ok := v.(string)
			if !ok || (op != "" && op != "APPEND" && op != "SET") {
				return nil
			}
			sub := fragmentPathPattern.FindStringSubmatch(p)[1]
			index, err := strconv.Atoi(sub)
			if err != nil {
				return errors.New("deepseekweb: fragment 下标非法")
			}
			if index == -1 {
				index = len(s.contents) - 1
			}
			if index < 0 || index >= len(s.kinds) {
				return errors.New("deepseekweb: 未知 fragment 下标")
			}
			// 上游既用 APPEND 也可能省略 o 表示增量；SET 才是快照。
			return s.writeFragment(index, text, op == "SET")
		default:
			if obj, ok := v.(map[string]any); ok {
				if list, has := obj["fragments"].([]any); has {
					return s.fragments(list, true)
				}
			}
			if strings.HasPrefix(p, "response/fragments/") && strings.HasSuffix(p, "/content") {
				return fmt.Errorf("deepseekweb: 不支持的内容补丁 %s", p)
			}
		}
		return nil
	}

	// p 为空：v 是纯文本（追加到最后一段）或整棵树。
	switch val := v.(type) {
	case string:
		if len(s.contents) == 0 {
			return errors.New("deepseekweb: 收到无归属的文本")
		}
		last := len(s.contents) - 1
		return s.writeFragment(last, val, false)
	case map[string]any:
		return s.tree(val)
	}
	return nil
}

// Event 处理一条 SSE 事件。
func (s *StreamState) Event(name, data string) error {
	if strings.EqualFold(name, "error") {
		return errors.New("deepseekweb: 流返回 error 事件")
	}
	var frame map[string]any
	if data != "" && data != "[DONE]" {
		parsed, err := decodeObject([]byte(data))
		if err != nil {
			return fmt.Errorf("deepseekweb: SSE 帧 JSON 非法: %w", err)
		}
		frame = parsed
		if err := checkFrameError(frame); err != nil {
			return err
		}
		if nested, ok := frame["v"].(map[string]any); ok {
			if err := checkFrameError(nested); err != nil {
				return err
			}
		}
	}
	if name == "close" {
		s.Closed = true
		return nil
	}
	if frame != nil && (name == "" || name == "message" || name == "ready") {
		return s.Apply(frame)
	}
	return nil
}

// ReadSSE 从响应流里读 SSE 并逐帧喂给 StreamState。收到 close 事件即返回。
func ReadSSE(r io.Reader, st *StreamState) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data strings.Builder

	dispatch := func() error {
		if data.Len() == 0 && event == "" {
			return nil
		}
		payload := data.String()
		name := event
		data.Reset()
		event = ""
		if err := st.Event(name, payload); err != nil {
			return err
		}
		if st.Closed {
			return errStreamDone
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > maxBody {
			return errors.New("deepseekweb: SSE 单行过长")
		}
		if len(line) > 0 {
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
				if derr := dispatch(); derr != nil && !errors.Is(derr, errStreamDone) {
					return derr
				}
				return nil
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// 一轮对话
// ---------------------------------------------------------------------------

// TurnResult 是一轮对话的结果。
type TurnResult struct {
	// SessionID 本轮使用的会话 id（调用方要存下来给下一轮）。
	SessionID string
	// Stream 累积状态。
	Stream *StreamState
}

// Run 跑一轮对话：准备请求 → （需要时建会话）→ PoW → POST completion → 解析流 →
// 处理 resume_stream / continue 的补偿路径。
//
// sessionID 传空表示新会话（本方法会建一个）；返回的 TurnResult.SessionID 就是会话 id。
// parentID 是上一轮的 assistant message_id，首轮传 nil。
// powTimeout 是单次 PoW 求解上限（≤120s，网页版给 30s 够用）。
func (c *Client) Run(ctx context.Context, chatBody []byte, sessionID string, parentID *int64, st *StreamState, powTimeout time.Duration) (*TurnResult, error) {
	if st == nil {
		st = NewStreamState(nil, nil)
	}
	// 先做纯本地校验，避免白建会话。
	if _, err := Prepare(chatBody, sessionID, parentID); err != nil {
		return nil, err
	}
	id := sessionID
	if id == "" {
		created, err := c.CreateSession(ctx)
		if err != nil {
			return nil, err
		}
		id = created
	}
	req, err := Prepare(chatBody, id, parentID)
	if err != nil {
		return nil, err
	}
	path := PathCompletion
	body := any(req)
	continues := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		extra := map[string]string{}
		if path == PathCompletion {
			challenge, err := c.FetchChallenge(ctx)
			if err != nil {
				return nil, err
			}
			answer, err := SolvePoW(ctx, challenge, powTimeout)
			if err != nil {
				return nil, err
			}
			header, err := PowHeader(challenge, answer)
			if err != nil {
				return nil, err
			}
			extra[HeaderPow] = header
		}

		st.Status = ""
		st.Closed = false
		sse, err := c.postStream(ctx, path, body, extra)
		if err != nil {
			return nil, err
		}
		if sse.contentType != "" && strings.Contains(strings.ToLower(sse.contentType), "text/event-stream") {
			readErr := ReadSSE(sse.reader, st)
			sse.close()
			if readErr != nil {
				return nil, readErr
			}
		} else {
			raw, err := io.ReadAll(io.LimitReader(sse.reader, maxBody))
			sse.close()
			if err != nil {
				return nil, err
			}
			root, err := decodeObject(raw)
			if err != nil {
				return nil, err
			}
			biz, err := unwrap(root, "completion", true)
			if err != nil {
				return nil, err
			}
			if biz == nil {
				return nil, errors.New("deepseekweb: completion 没有返回流")
			}
			response, _ := biz["response"].(map[string]any)
			if response == nil {
				return nil, errors.New("deepseekweb: completion 没有返回 response")
			}
			if err := st.Apply(map[string]any{"v": map[string]any{"response": response}}); err != nil {
				return nil, err
			}
			if st.Status != "FINISHED" {
				return nil, fmt.Errorf("deepseekweb: JSON 响应未完成（status=%s）", st.Status)
			}
			return &TurnResult{SessionID: id, Stream: st}, nil
		}

		// 首轮没吐任何内容就 FINISHED：上游偶尔会空转，重放一次 resume_stream。
		if st.Status == "FINISHED" && st.Text.Len() == 0 && st.Reasoning.Len() == 0 && st.HasMessageID && continues == 0 {
			path = PathResumeStream
			body = map[string]any{"chat_session_id": id, "message_id": st.MessageID}
			continues++
			continue
		}
		// 上游主动截断：续写。
		if st.Status == "INCOMPLETE" && st.HasMessageID && continues < maxContinues {
			path = PathContinue
			body = map[string]any{
				"chat_session_id":    id,
				"message_id":         st.MessageID,
				"fallback_to_resume": true,
			}
			continues++
			continue
		}
		if st.Status != "FINISHED" {
			return nil, fmt.Errorf("deepseekweb: 流未以 FINISHED 结束（status=%s）", st.Status)
		}
		return &TurnResult{SessionID: id, Stream: st}, nil
	}
}

// stream 是一次打开的流式响应。
type streamResponse struct {
	reader      io.Reader
	contentType string
	close       func()
}

// postStream 发起 completion / resume / continue 请求并返回可读流。
func (c *Client) postStream(ctx context.Context, path string, body any, extra map[string]string) (*streamResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	base := c.BaseURL
	if base == "" {
		base = Origin
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range Headers(&Account{Token: c.Token}) {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if client == nil {
		client = HTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited, truncated := readLimited(resp.Body, maxBody)
		resp.Body.Close()
		mapped := 502
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			mapped = http.StatusUnauthorized
		case http.StatusTooManyRequests:
			mapped = http.StatusTooManyRequests
		}
		return nil, &UpstreamError{
			Message:        fmt.Sprintf("deepseekweb: completion HTTP %d", resp.StatusCode),
			UpstreamStatus: resp.StatusCode,
			GatewayStatus:  mapped,
			Body:           string(limited),
			BodyTruncated:  truncated,
		}
	}
	return &streamResponse{
		reader:      resp.Body,
		contentType: resp.Header.Get("Content-Type"),
		close:       func() { resp.Body.Close() },
	}, nil
}

// ---------------------------------------------------------------------------
// 通用小工具
// ---------------------------------------------------------------------------

// decodeObject 解 JSON 对象并保留数字精度。
func decodeObject(b []byte) (map[string]any, error) {
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// cloneMap 浅拷贝。
func cloneMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// pickString 取字符串字段；数字/布尔会被转成字符串（上游 code 字段有时是数字）。
func pickString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		if v {
			return "true"
		}
		return "false"
	}
	return ""
}

// pickInt 取整数字段。
func pickInt(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, err := asInt(m[key])
	if err != nil {
		return 0
	}
	return v
}

// pickBool 取布尔字段，缺失时返回默认值。
func pickBool(m map[string]any, key string, def bool) bool {
	if m == nil {
		return def
	}
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
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
	return 0, errors.New("deepseekweb: 不是数字")
}

// quoteJSON 转义字符串。
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// containsString 判断切片里是否有该值。
func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
