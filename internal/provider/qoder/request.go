// 本文件负责 Qoder 协议里「请求体」与「响应流」这两块翻译：
//
//  1. BuildChatBody：标准 OpenAI chat 请求 → Qoder 的信封请求体 → EncodeBody 后的线上串。
//  2. RelayStream / Aggregate：Qoder 的 SSE 事件流 → 标准 OpenAI 的 chunk 序列 / 一个
//     chat.completion 对象。
//
// 字段结构照抄桌面端（国内站 CLI）实际发出的那份信封：body 一层是 agent 会话信息，
// business 里带 product/version/type/id/begin_at/stage，参数在 parameters，
// 模型在 model_config，system 单独一段，messages 里第一条是 system 回合。
package qoder

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// 信封里的几个默认值。
const (
	// DefaultMaxTokens 是客户端没给 max_tokens 时的默认上限（与桌面端一致）。
	DefaultMaxTokens = 32000
	// DefaultModelSource 是 model_config.source 的兜底：Qoder 自建模型标 system，
	// 走第三方中转的模型才需要别的值。
	DefaultModelSource = "system"
	// DefaultAgentID / DefaultTaskID / DefaultSessionType 是 agent 通道的固定值，
	// 桌面端不随模型变化，客户端也改不了。
	DefaultAgentID     = "agent_common"
	DefaultTaskID      = "common"
	DefaultSessionType = "app"
	// sessionDomain 参与 session_id 的派生，避免和别的供应商撞同一个摘要空间。
	sessionDomain = "allin/qoder/session"
)

// ---------- 入参：标准 OpenAI chat 请求 ----------

// inMessage 是客户端发来的一条消息（只关心文本主线需要的字段）。
type inMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// inTool 是客户端声明的一个工具。
type inTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type inChatRequest struct {
	Messages []inMessage `json:"messages"`
	Tools    []inTool    `json:"tools"`
	// 用 float64 接：有的客户端把 max_tokens 写成 2048.0 这类小数，用 *int 会直接解失败。
	MaxTokens           *float64 `json:"max_tokens"`
	MaxCompletionTokens *float64 `json:"max_completion_tokens"`
	User                string   `json:"user"`
	PromptCacheKey      string   `json:"prompt_cache_key"`
}

// ---------- 出参：Qoder 信封 ----------

// textBlock 是 content / contents 里的一个文本块。
type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// outMessage 是信封里的一条消息。
//
// user 回合必须同时带 content 和 contents 两份同样的块：桌面端给最后一条用户消息
// 打缓存标记（cache_control）后就把它挪进 contents，缺了这份上游可能直接 500。
type outMessage struct {
	Role       string      `json:"role"`
	Content    []textBlock `json:"content"`
	Contents   []textBlock `json:"contents,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

// outModelConfig 对应桌面端的 model_config。
type outModelConfig struct {
	Key            string `json:"key"`
	DisplayName    string `json:"display_name"`
	Model          string `json:"model"`
	Format         string `json:"format"`
	IsVL           bool   `json:"is_vl"`
	IsReasoning    bool   `json:"is_reasoning"`
	APIKey         string `json:"api_key"`
	URL            string `json:"url"`
	Source         string `json:"source"`
	MaxInputTokens int    `json:"max_input_tokens"`
}

// outBusiness 是信封的 business 段，agent 会话的身份与生命周期都在这里。
type outBusiness struct {
	Product  string `json:"product"`
	Version  string `json:"version"`
	Type     string `json:"type"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	BeginAt  int64  `json:"begin_at"`
	Stage    string `json:"stage"`
	ModelKey string `json:"model_key"`
}

// outParameters 是采样参数（Qoder 只认这几项，温度之类不认，不要原样塞进去）。
type outParameters struct {
	EnableThinking bool `json:"enable_thinking"`
	MaxTokens      int  `json:"max_tokens"`
	// ContextLength 需要模型目录里声明的窗口才能填，这里没有目录，省略让上游用默认值。
	ContextLength int `json:"context_length,omitempty"`
}

// outTool 是信封里的工具声明，形状与 OpenAI 的 function 一致。
type outTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// chatEnvelope 是最终发给上游的请求体（再过 EncodeBody 才是线上串）。
type chatEnvelope struct {
	RequestID    string         `json:"request_id"`
	RequestSetID string         `json:"request_set_id"`
	ChatRecordID string         `json:"chat_record_id"`
	SessionID    string         `json:"session_id"`
	Parameters   outParameters  `json:"parameters"`
	Business     outBusiness    `json:"business"`
	AgentID      string         `json:"agent_id"`
	TaskID       string         `json:"task_id"`
	SessionType  string         `json:"session_type"`
	ModelConfig  outModelConfig `json:"model_config"`
	System       []textBlock    `json:"system"`
	Messages     []outMessage   `json:"messages"`
	Tools        []outTool      `json:"tools,omitempty"`
}

// BuildChatBody 把客户端发来的标准 OpenAI chat 请求体翻译成 Qoder 的信封，并返回
// EncodeBody 之后的线上串（可以直接当请求体发出去）。
//
// modelKey 是 Qoder 模型目录里的模型 key（例如 X-Model-Key 头或模型名本身）；
// 它同时写进 model_config.key 和 business.model_key，两处必须一致，否则上游拒。
// modelSource 是 model_config.source，留空按 DefaultModelSource 处理。
//
// 这里是「文本主线」的移植：图片块、reasoning_effort 分级、工具调用配对的完整规则
// 都还没搬（见文件末尾的限制说明），文本对话与工具声明已经够用。
func BuildChatBody(chatBody []byte, modelKey, modelSource string) (string, error) {
	var req inChatRequest
	if err := json.Unmarshal(chatBody, &req); err != nil {
		return "", fmt.Errorf("qoder: 请求体不是合法 JSON: %w", err)
	}
	if len(req.Messages) == 0 {
		return "", errors.New("qoder: 请求体缺少 messages")
	}

	modelKey = strings.TrimSpace(modelKey)
	if modelKey == "" {
		return "", errors.New("qoder: 缺少模型 key（模型名或 X-Model-Key 头里要给出模型目录里的 key）")
	}
	// auto / default 是目录里的占位项，不能当真实 key 发；这是桌面端自己的规矩。
	if strings.EqualFold(modelKey, "auto") || strings.EqualFold(modelKey, "default") {
		return "", fmt.Errorf("qoder: 模型 key %q 是占位项，请指定具体模型", modelKey)
	}
	modelSource = strings.TrimSpace(modelSource)
	if modelSource == "" {
		modelSource = DefaultModelSource
	}

	// ---- 1. system 合并：桌面端固定前置一段 agent 提示，再拼客户端自己的 system/developer ----
	sysText := QoderSystem
	var userSys []string
	for _, m := range req.Messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if t := contentText(m.Content); t != "" {
				userSys = append(userSys, t)
			}
		}
	}
	if len(userSys) > 0 {
		sysText += "\n\n" + strings.Join(userSys, "\n\n")
	}
	sysBlock := textBlock{Type: "text", Text: sysText}

	// ---- 2. messages：第一条固定是 system 回合，其余按文本块平铺 ----
	out := make([]outMessage, 0, len(req.Messages)+1)
	out = append(out, outMessage{Role: "system", Content: []textBlock{sysBlock}})
	lastUserAt, lastUserText := -1, ""
	for i, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			continue
		}
		text := contentText(m.Content)
		if role == "user" {
			lastUserAt, lastUserText = i, text
		}
		if text == "" {
			continue
		}
		block := []textBlock{{Type: "text", Text: text}}
		msg := outMessage{Role: role, Content: block}
		if role == "user" {
			// user 回合要同时带 contents，见 outMessage 上的说明。
			msg.Contents = block
		}
		if role == "tool" {
			msg.ToolCallID = strings.TrimSpace(m.ToolCallID)
		}
		out = append(out, msg)
	}

	// ---- 3. 工具：类型/名字/参数直接沿用，缺参数就补一个空 object schema ----
	tools := make([]outTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			continue
		}
		var one outTool
		one.Type = "function"
		one.Function.Name = name
		one.Function.Description = t.Function.Description
		params := strings.TrimSpace(string(t.Function.Parameters))
		if params == "" || params == "null" {
			params = `{"type":"object","properties":{}}`
		}
		one.Function.Parameters = json.RawMessage(params)
		tools = append(tools, one)
	}

	// ---- 4. 会话 / 任务标识 ----
	sessionID := sessionIDFor(&req)
	// request_set_id 是「一次任务」的 id：同一个任务（同一段用户输入）重发时不变，
	// 桌面端就是这么用它做重试与去重的。这里按 session + 最后一条用户输入派生。
	requestSetID := digestUUID("task\x00" + sessionID + "\x00" + strconv.Itoa(lastUserAt) + "\x00" + lastUserText)
	requestID, err := randomUUID()
	if err != nil {
		return "", fmt.Errorf("qoder: 生成 request_id 失败: %w", err)
	}

	// ---- 5. 参数 ----
	maxTokens := DefaultMaxTokens
	if n := positiveInt(req.MaxTokens); n > 0 {
		maxTokens = n
	} else if n := positiveInt(req.MaxCompletionTokens); n > 0 {
		maxTokens = n
	}

	now := time.Now().UnixMilli()
	env := chatEnvelope{
		RequestID:    requestID,
		RequestSetID: requestSetID,
		ChatRecordID: requestID,
		SessionID:    sessionID,
		Parameters: outParameters{
			// 桌面端默认开着思考；这里不发 reasoning_effort，让上游用模型自己的默认档。
			EnableThinking: true,
			MaxTokens:      maxTokens,
		},
		Business: outBusiness{
			Product:  "app",
			Version:  CosyVersion,
			Type:     "agent",
			ID:       sessionID,
			Name:     taskName(lastUserText),
			BeginAt:  now,
			Stage:    "start",
			ModelKey: modelKey,
		},
		AgentID:     DefaultAgentID,
		TaskID:      DefaultTaskID,
		SessionType: DefaultSessionType,
		ModelConfig: outModelConfig{
			Key:         modelKey,
			DisplayName: modelKey,
			Model:       "",
			Format:      "openai",
			IsVL:        false,
			// 模型目录里才有 is_reasoning，这里不知道，保守标 false。
			IsReasoning: false,
			APIKey:      "",
			URL:         "",
			Source:      modelSource,
			// 窗口大小同理来自目录，缺省按 20 万给（不会真的下发，只是字段必填）。
			MaxInputTokens: 200000,
		},
		System:   []textBlock{sysBlock},
		Messages: out,
		Tools:    tools,
	}

	raw, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("qoder: 信封序列化失败: %w", err)
	}
	return EncodeBody(string(raw)), nil
}

// contentText 从 content 里抽纯文本：兼容 string 与 [{type:text,text:...}] 两种形态。
//
// 图片块这里直接丢掉（Qoder 只在声明了 is_vl 的模型上收图，而目录信息不在本包里）。
func contentText(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// sessionIDFor 派生 session_id：优先 prompt_cache_key，其次 user，最后用「对话开头」
// （全部 system 文本 + 第一条用户输入）的摘要。三者都没有才现造一个随机 UUID。
//
// 为什么不用纯随机：同一段对话每一轮都要落到同一个 session_id，上游才命中缓存、
// 多轮上下文也才连得上。
func sessionIDFor(req *inChatRequest) string {
	if v := strings.TrimSpace(req.PromptCacheKey); v != "" {
		return digestUUID("key\x00" + v)
	}
	if v := strings.TrimSpace(req.User); v != "" {
		return digestUUID("user\x00" + v)
	}
	var sys []string
	firstUser := ""
	for _, m := range req.Messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			sys = append(sys, contentText(m.Content))
		case "user":
			if firstUser == "" {
				firstUser = contentText(m.Content)
			}
		}
	}
	opening := strings.Join(sys, "\x00") + "\x00" + firstUser
	if strings.TrimSpace(opening) != "" {
		return digestUUID("chat\x00" + opening)
	}
	if v, err := randomUUID(); err == nil {
		return v
	}
	return "00000000-0000-4000-8000-000000000000"
}

// digestUUID 把一段种子摘要成 UUID 形状的串（确定性，同样的种子永远同一个值）。
func digestUUID(seed string) string {
	sum := sha256.Sum256([]byte(sessionDomain + "\x00" + seed))
	h := hex.EncodeToString(sum[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// positiveInt 把「可能带小数的正数」收敛成一个整数，非正/缺失一律返回 0。
func positiveInt(v *float64) int {
	if v == nil || *v <= 0 || *v > 1<<20 {
		return 0
	}
	return int(*v)
}

// taskName 是 business.name：桌面端取用户这句话的前 10 个字当任务名（纯展示用）。
func taskName(prompt string) string {
	r := []rune(prompt)
	if len(r) > 10 {
		return string(r[:10])
	}
	return string(r)
}

// ================= 响应侧：SSE 解码 =================

// APIError 之外，这里只多一类「帧坏了」的错误，沿用 errors.New 即可。

// RelayStream 读 Qoder 的 SSE 事件流，把解出来的标准 OpenAI chunk 逐帧写到 dst。
//
// 上游每帧 data 还套了一层 {"statusCodeValue":200,"body":"<chunk json>"} 信封，
// 而且 body 有可能是自定义 base64（请求带 Encode=1 时），所以必须先解帧再透传；
// 写出去的是干净的 `data: {...}\n\n`，客户端按标准 OpenAI 流式解析即可。
func RelayStream(dst io.Writer, src io.Reader) error {
	return walkEvents(src, func(payload string) error {
		_, err := io.WriteString(dst, "data: "+payload+"\n\n")
		return err
	})
}

// Aggregate 把整条 Qoder 事件流聚合成一个标准的 chat.completion 对象。
//
// 客户端要非流式（stream:false）时用它：上游只给流，聚合只能在本地做。
func Aggregate(src io.Reader, model string) ([]byte, error) {
	var (
		text, reasoning strings.Builder
		calls           = map[int]*aggCall{}
		order           []int
		chunkID         string
		created         int64
		modelName       string
		finish          string
		usage           json.RawMessage
		business        bool
	)
	err := walkEvents(src, func(payload string) error {
		if payload == "[DONE]" {
			return nil
		}
		var chunk struct {
			ID      string          `json:"id"`
			Created int64           `json:"created"`
			Model   string          `json:"model"`
			Usage   json.RawMessage `json:"usage"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("qoder: 上游 chunk 解析失败: %w", err)
		}
		if chunk.ID != "" && chunkID == "" {
			chunkID = chunk.ID
		}
		if chunk.Created != 0 && created == 0 {
			created = chunk.Created
		}
		if chunk.Model != "" && modelName == "" {
			modelName = chunk.Model
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				text.WriteString(c.Delta.Content)
				business = true
			}
			if c.Delta.ReasoningContent != "" {
				reasoning.WriteString(c.Delta.ReasoningContent)
			}
			for _, tc := range c.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				call := calls[idx]
				if call == nil {
					call = &aggCall{}
					calls[idx] = call
					order = append(order, idx)
				}
				if tc.ID != "" {
					call.id = tc.ID
				}
				if tc.Function.Name != "" {
					call.name = tc.Function.Name
				}
				call.args.WriteString(tc.Function.Arguments)
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !business && len(order) == 0 {
		return nil, errors.New("qoder: 上游没有返回任何内容")
	}
	if chunkID == "" {
		chunkID = "chatcmpl-qoder"
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	if modelName == "" {
		modelName = model
	}

	message := map[string]any{"role": "assistant"}
	if text.Len() > 0 {
		message["content"] = text.String()
	} else {
		message["content"] = nil
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(order) > 0 {
		list := make([]any, 0, len(order))
		for i, idx := range order {
			call := calls[idx]
			if strings.TrimSpace(call.name) == "" {
				continue
			}
			list = append(list, map[string]any{
				"index": i,
				"id":    call.id,
				"type":  "function",
				"function": map[string]any{
					"name":      call.name,
					"arguments": call.args.String(),
				},
			})
		}
		if len(list) > 0 {
			message["tool_calls"] = list
			finish = "tool_calls"
		}
	}
	if finish == "" {
		finish = "stop"
	}

	result := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion",
		"created": created,
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if len(usage) > 0 {
		result["usage"] = usage
	}
	return json.Marshal(result)
}

// aggCall 是非流式聚合时攒起来的一个工具调用。
type aggCall struct {
	id   string
	name string
	args strings.Builder
}

// walkEvents 按 SSE 规则切帧，把每帧解出来的 payload 交给 fn（"[DONE]" 原样传）。
func walkEvents(src io.Reader, fn func(string) error) error {
	reader := bufio.NewReaderSize(src, 32*1024)
	var data strings.Builder
	hasData := false

	flush := func() error {
		if !hasData {
			return nil
		}
		payload := data.String()
		data.Reset()
		hasData = false
		return dispatch(payload, fn)
	}

	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			// 空行 = 一帧结束（也可能只是开头/结尾的空行，flush 里有 hasData 兜着）。
			if ferr := flush(); ferr != nil {
				return ferr
			}
		case strings.HasPrefix(line, "data:"):
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(v)
			hasData = true
		default:
			// event: / id: / retry: / ":" 注释行都不是数据，忽略。
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	return flush()
}

// dispatch 解一帧：可能是 [DONE]，可能是裸 chunk，也可能是 {statusCodeValue, body} 信封。
func dispatch(payload string, fn func(string) error) error {
	text := strings.TrimSpace(payload)
	if text == "" {
		return nil
	}
	if text == "[DONE]" {
		return fn("[DONE]")
	}
	inner, err := unwrap(text)
	if err != nil {
		return err
	}
	if inner == "" {
		return nil
	}
	return fn(inner)
}

// unwrap 把一帧拆成内部 chunk 的 JSON 文本（空串表示这帧没有业务内容）。
//
// 解出来的顺序是：非 JSON 就先过 DecodeBody（Encode=1 时上游可能整体编码），
// 再判断是不是 {statusCodeValue, body} 信封；body 里若还不是 JSON，同样再过一次
// DecodeBody。这样「编码/不编码」「有信封/没信封」四种组合都能吃。
func unwrap(text string) (string, error) {
	text = normalize(text)
	if text == "" {
		return "", nil
	}
	if text == "[DONE]" {
		return "[DONE]", nil
	}
	if !strings.HasPrefix(text, "{") {
		return "", fmt.Errorf("qoder: 上游事件帧无法解析: %s", snippet([]byte(text)))
	}

	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return "", fmt.Errorf("qoder: 上游事件帧不是合法 JSON: %w", err)
	}

	// 信封形态：{statusCodeValue, body}
	if rawStatus, ok := env["statusCodeValue"]; ok {
		var code int
		if err := json.Unmarshal(rawStatus, &code); err != nil {
			return "", errors.New("qoder: 上游事件帧的状态码无效")
		}
		body := ""
		if rawBody, ok2 := env["body"]; ok2 {
			if err := json.Unmarshal(rawBody, &body); err != nil {
				body = string(rawBody)
			}
		}
		body = normalize(body)
		if code != 200 {
			if body == "" {
				body = text
			}
			return "", &APIError{Status: code, Msg: body}
		}
		if body == "[DONE]" {
			return "[DONE]", nil
		}
		if body == "" {
			return "", nil
		}
		return body, nil
	}

	// 错误帧：{"error": {...}} 或 {"code": ..., "message": ...}
	if raw, ok := env["error"]; ok && len(raw) > 0 && string(raw) != "null" {
		return "", &APIError{Status: 502, Msg: text}
	}
	if _, ok := env["code"]; ok {
		if _, hasMsg := env["message"]; hasMsg {
			if _, hasChoices := env["choices"]; !hasChoices {
				return "", &APIError{Status: 502, Msg: text}
			}
		}
	}
	// 裸 chunk（上游本来就直接给 OpenAI 形态）或其它我们不认识的结构：认识的往下传，
	// 不认识的当空帧丢掉，别把非 chunk 的东西写进客户端的事件流。
	if _, ok := env["choices"]; ok {
		return text, nil
	}
	return "", nil
}

// normalize 把「可能是自定义 base64」的文本还原成 JSON 文本（认不出来就原样返回）。
func normalize(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "{") || text == "[DONE]" {
		return text
	}
	decoded := strings.TrimSpace(DecodeBody(text))
	if strings.HasPrefix(decoded, "{") || decoded == "[DONE]" {
		return decoded
	}
	return text
}

// ---------- 未移植的部分（明确的已知限制） ----------
//
//  1. 图片：客户端发来的 image_url 块会被丢掉，Qoder 只有 is_vl 模型收图，而模型目录
//     不在本包里（Models() 才拉），这里没有可靠的开关。
//  2. reasoning_effort：不发。上游支持分级（none…ultra），但具体级别来自模型目录，
//     盲发可能被拒，所以让模型用默认档。
//  3. 工具调用配对：历史里的 assistant.tool_calls / tool.tool_call_id 只是原样保留，
//     没有做「补 id、按顺序配对、补不上的降级成用户回合」那套规范化。
//  4. 上游报「max_tokens 超过模型上限」时不会自动降档重发，直接把错误交给客户端。
