// protocol.go：Trae 上游事件流 → OpenAI chat.completion.chunk 的协议转换。
//
// # 一、上游是什么
//
// 上游 endpoint 是 https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat
// （POST，请求头 Accept: text/event-stream），返回标准 SSE：
//
//	event: <事件名>\n
//	data: <JSON 对象，少数辅助事件是裸字符串>\n
//	\n
//
// 事件名不是固定枚举，因此上游送来什么就按下面三步归一化：
// camelCase 拆成 snake_case → 非字母数字都换成 '_' → 转小写并去掉首尾 '_'。
// 若 event 为空或等于 "message"，则从 JSON 载荷里的 "event"（没有则 "type"）取事件名。
//
// 逐事件看清语义后可以分四类：
//
//  1. 辅助/遥测（必须丢弃）：progress_notice、progress_info、progress、queue、queued、
//     metadata、timing、timing_cost、heartbeat、ping、extra_info。
//     这类事件的载荷常常是裸字符串（"progress_notice" 就是），不是 JSON 对象，
//     所以判据是「事件名在辅助表里 && 载荷不以 '{' 开头」→ 直接丢。
//     若载荷是对象，则只有当它不含任何语义字段时才丢，否则报「unsupported semantic event」。
//
//  2. 正文增量：事件名为空或 output/message/delta/content/text/answer/chunk/
//     output_delta/message_delta。载荷里取正文的优先级是
//     response → content → delta.content → delta(字符串)。
//     注意 response 是 Trae 自己的正文字段，而且**可能是累积值**而不是增量。
//
//  3. 用量与终止：token_usage（只更新用量）；done / response_done / stream_done /
//     stream_finish（收尾并终止）；data 整帧等于 "[DONE]" 也直接终止。
//
//  4. 错误：事件名 error，或载荷带 error 字段，或带 ResponseMetadata.Error，
//     或 code 非 0 且带 message/msg 而既没有 response 也没有 content。
//
// 正文里还可能夹着三种「文本形式」的工具调用语法（Trae 模型有时不走原生 tool_calls）：
//
//	<tool_call>{"name":..,"arguments":..}</tool_call>             // 通用块 / GLM 的 <arg_key>
//	<function=name><parameter=k>v</parameter></function>           // Qwen
//	<function_calls><invoke name="name"><parameter name="k">v</parameter></invoke></function_calls>
//	                                                              // Anthropic / antml
//
// 这三种语法只对「本次请求声明过的工具」生效；写在行内代码 ` 或代码围栏 ``` 里的一律当正文。
//
// # 二、本文件的产出
//
// 单事件：TransformEvent(line, state) 把一行 SSE（或一行裸载荷）喂进状态机，
// 返回 0..n 个已经序列化好的 OpenAI SSE 帧（每帧形如 "data: {...}\n\n"）。
//
// 整条流：ConvertStream(r, model) 吃完整条上游流，返回一个标准 chat.completion 的 JSON。
//
// # 三、保留的硬限制（与 Java 版一致）
//
//   - 单个 SSE 载荷 1 MiB（LimitFrame）
//   - 累计数据 / 累计输出 8 MiB（LimitTotal）
//   - 工具调用最多 128 个（MaxCalls）
//   - 单条工具调用 name+arguments 缓冲 1 MiB，文本缓冲 1 MiB，思考缓冲 1 MiB

package trae

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// LimitFrame 单个 SSE 载荷上限：超过即判定上游异常。
	LimitFrame = 1 << 20
	// LimitTotal 累计数据、累计输出上限。
	LimitTotal = 8 << 20
	// MaxCalls 一条回复里允许的工具调用个数上限。
	MaxCalls = 128
)

// auxiliaryEvents 是上游随答案一起发的进度/遥测事件，全部丢弃。
// 它们不带正文，其中若干（progress_notice 等）载荷是裸字符串。
var auxiliaryEvents = map[string]struct{}{
	"progress_notice": {},
	"progress_info":   {},
	"progress":        {},
	"queue":           {},
	"queued":          {},
	"metadata":        {},
	"timing":          {},
	"timing_cost":     {},
	"heartbeat":       {},
	"ping":            {},
	"extra_info":      {},
}

// textEvents 是「正文增量」事件的合法名字集合（含事件名为空的情况）。
var textEvents = map[string]struct{}{
	"":              {},
	"output":        {},
	"message":       {},
	"delta":         {},
	"content":       {},
	"text":          {},
	"answer":        {},
	"chunk":         {},
	"output_delta":  {},
	"message_delta": {},
}

// ---------------------------------------------------------------------------
// 错误类型
// ---------------------------------------------------------------------------

// ProtocolError 表示上游流不符合协议（帧被截断、工具调用不完整、超限等）。
type ProtocolError struct{ Reason string }

func (e *ProtocolError) Error() string { return "Trae protocol: " + e.Reason }

func protoErr(reason string) error { return &ProtocolError{Reason: reason} }

// UpstreamError 是上游明确报错（code / message），映射成对客户端有意义的状态码。
type UpstreamError struct {
	Code    string // 上游原始 code，非数字时为 "unknown"
	Status  int    // 建议回给客户端的 HTTP 状态
	Message string // 已脱敏的中文提示
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("Trae 上游错误 %s（HTTP %d）：%s", e.Code, e.Status, e.Message)
}

// ---------------------------------------------------------------------------
// OpenAI 侧的输出结构
// ---------------------------------------------------------------------------

type chunkDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallDelta `json:"tool_calls,omitempty"`
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Name     string `json:"name,omitempty"`
	ArgsText string `json:"arguments,omitempty"`
}

func (t toolCallDelta) MarshalJSON() ([]byte, error) {
	// 严格按 OpenAI 的形状输出：function 必须是个对象，且 name/arguments 按需出现。
	fn := map[string]any{}
	if t.Name != "" {
		fn["name"] = t.Name
	}
	if t.ArgsText != "" {
		fn["arguments"] = t.ArgsText
	}
	out := map[string]any{"index": t.Index, "function": fn}
	if t.ID != "" {
		out["id"] = t.ID
	}
	if t.Type != "" {
		out["type"] = t.Type
	}
	return json.Marshal(out)
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkEnvelope struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []chunkChoice  `json:"choices"`
	Usage   map[string]any `json:"usage,omitempty"`
}

// ToolSpec 是「本次请求声明过的工具」，文本形式的工具调用只在声明过时才被识别。
type ToolSpec struct {
	Name       string
	Parameters json.RawMessage // 工具参数 JSON Schema，可为空
}

// toolCall 是缓冲中的一条工具调用（对应 Java 的 Call）。
type toolCall struct {
	ID    string
	Name  string
	Args  string
	Index *int
}

// codeState 记录「到目前为止吐出的正文」是否停在行内代码或代码围栏里，
// 这些位置里的 <tool_call>/<function=…> 一律当正文（与上游插件行为一致）。
type codeState struct {
	fences, ticks, run int
}

func (c *codeState) feed(s string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			c.run++
			continue
		}
		c.settle()
		if s[i] == '\n' {
			c.ticks = 0
		}
	}
}

func (c *codeState) settle() {
	if c.run > 0 {
		c.fences += c.run / 3
		c.ticks += c.run % 3
		c.run = 0
	}
}

func (c *codeState) quoted() bool {
	return ((c.fences+c.run/3)&1) == 1 || ((c.ticks+c.run%3)&1) == 1
}

// ---------------------------------------------------------------------------
// 流状态机
// ---------------------------------------------------------------------------

// StreamState 是一条上游流的全部可变状态。一个流一个实例，不可并发复用。
type StreamState struct {
	Model   string
	ChunkID string
	Created int64

	// 声明过的工具：名字 + 各工具 schema 的 properties（用来决定参数按字符串还是 JSON 读）
	names map[string]struct{}
	props map[string]map[string]any

	// 已缓冲的工具调用（终止时才整体输出，中途任何一步校验失败都让整条流失败）
	calls []*toolCall

	// 正文缓冲：还没决定是正文还是工具标记的那部分文本
	text      string
	quoted    codeState
	afterCall bool

	// 思考：thought 是已吐出的累计思考，lastThought 是上一次收到的原始字符串
	thought     string
	lastThought string

	// 已吐出的内容，供非流式聚合使用
	contentOut   strings.Builder
	reasoningOut strings.Builder

	// 限制计数
	size      int // 累计 data 字节
	frameSize int // 当前帧已读字节
	skipped   int // 连续空帧跳过的字节
	outBytes  int // 已产生的输出字节

	// 结果
	Terminal     bool
	Ended        bool
	FinishReason string
	Usage        map[string]any

	// SSE 分帧
	pendingEvent string
	pendingData  strings.Builder
	hasData      bool
}

// NewStreamState 创建一个流状态。model 是回给客户端的模型名。
func NewStreamState(model string) *StreamState {
	return &StreamState{
		Model:   model,
		ChunkID: "chatcmpl-" + randomHex(16),
		Created: time.Now().Unix(),
		names:   map[string]struct{}{},
		props:   map[string]map[string]any{},
	}
}

// DeclareTools 声明本次请求的工具集：只有声明过的工具名的文本形式调用才会被识别，
// 且终止时会校验工具名与参数 JSON 合法性。
func (st *StreamState) DeclareTools(tools []ToolSpec) error {
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		st.names[name] = struct{}{}
		raw := strings.TrimSpace(string(t.Parameters))
		if raw == "" {
			st.props[name] = nil
			continue
		}
		// parameters 可能是对象，也可能是「对象的 JSON 字符串」，两种都吃。
		var schema map[string]any
		if err := json.Unmarshal([]byte(raw), &schema); err != nil {
			var asString string
			if err2 := json.Unmarshal([]byte(raw), &asString); err2 == nil {
				if err2 = json.Unmarshal([]byte(asString), &schema); err2 != nil {
					schema = nil
				}
			} else {
				schema = nil
			}
		}
		if schema == nil {
			st.props[name] = nil
			continue
		}
		if p, ok := schema["properties"].(map[string]any); ok {
			st.props[name] = p
		} else {
			st.props[name] = nil
		}
	}
	return nil
}

func (st *StreamState) hasName(name string) bool {
	_, ok := st.names[name]
	return ok
}

// ---------------------------------------------------------------------------
// 对外入口 1：单事件转换
// ---------------------------------------------------------------------------

// TransformEvent 吃一行上游流文本，返回 0..n 个可直接写给客户端的 SSE 帧。
//
// 支持三种喂法，方便调用方按自己的读法选择：
//   - 标准 SSE 逐行：event: xxx / data: {...} / 空行（空行触发派发）
//   - 只有 data 行：data: {...} 之后再来一次 data: 或空行都不影响
//   - 直接喂一行裸载荷：{"response":"hi"}（没有 "data:" 前缀时按整条载荷立即派发）
//
// 返回的每个字符串都是一帧完整的 "data: {...}\n\n"（终止时含 "data: [DONE]\n\n"）。
func TransformEvent(line string, state *StreamState) ([]string, error) {
	if state == nil {
		return nil, protoErr("missing stream state")
	}
	if state.Terminal {
		// 终止之后再来的事件按协议错误处理：上游已经说完了还在发东西。
		return nil, protoErr("event after terminal")
	}
	line = strings.TrimSuffix(line, "\r")

	// 空行 = 一帧结束。注释行（": ping" 之类）丢弃。
	if line == "" {
		return state.dispatch()
	}
	if strings.HasPrefix(line, ":") {
		return nil, nil
	}

	state.frameSize += len(line) + 1
	if state.frameSize > LimitFrame {
		return nil, protoErr("事件超过大小限制")
	}

	key, value, hasColon := strings.Cut(line, ":")
	if hasColon {
		switch key {
		case "event":
			state.pendingEvent = strings.TrimPrefix(value, " ")
			return nil, nil
		case "data":
			value = strings.TrimPrefix(value, " ")
			if state.hasData {
				state.pendingData.WriteByte('\n')
			}
			state.pendingData.WriteString(value)
			state.hasData = true
			return nil, nil
		case "id", "retry":
			// 上游有时带 id/retry，本协议用不上。
			return nil, nil
		}
	}

	// 没有 SSE 前缀：把这一行当成整条载荷立即派发（兼容「一行一个 JSON」的上游）。
	state.pendingData.WriteString(line)
	state.hasData = true
	return state.dispatch()
}

// Flush 在流结束时调用：把最后一个数据帧（没有以空行收尾的那个）补派发掉。
// 若还有半截帧，返回错误。
func Flush(state *StreamState) ([]string, error) {
	if state == nil || !state.hasData {
		return nil, nil
	}
	return state.dispatch()
}

// dispatch 把 pendingEvent + pendingData 交给 accept，并在终止时补用量帧与 [DONE]。
func (st *StreamState) dispatch() ([]string, error) {
	st.frameSize = 0
	if !st.hasData {
		st.skipped += st.frameSize
		if st.skipped > LimitTotal {
			return nil, protoErr("流缺少有效数据")
		}
		return nil, nil
	}
	body := st.pendingData.String()
	event := st.pendingEvent
	st.pendingEvent = ""
	st.pendingData.Reset()
	st.hasData = false
	st.skipped = 0

	// 事件名为空或 "message" 时，事件名其实藏在载荷里（event 或 type 字段）。
	if event == "" || event == "message" {
		if body != "[DONE]" {
			if obj, err := parseObject(body); err == nil {
				if v, ok := obj["event"]; ok {
					if s, ok := v.(string); ok && s != "" {
						event = s
					}
				} else if v, ok := obj["type"]; ok {
					if s, ok := v.(string); ok && s != "" {
						event = s
					}
				}
			}
		}
	}

	frames, err := st.accept(event, body)
	if err != nil {
		return nil, err
	}
	if st.Terminal {
		if st.Usage != nil {
			// 与 OpenAI 一致：流末尾补一帧只带 usage 的分片（choices 为空）。
			u, err := st.encode(chunkEnvelope{
				ID:      st.ChunkID,
				Object:  "chat.completion.chunk",
				Created: st.Created,
				Model:   st.Model,
				Choices: []chunkChoice{},
				Usage:   st.Usage,
			})
			if err != nil {
				return nil, err
			}
			frames = append(frames, u...)
		}
		done, err := st.account("data: [DONE]\n\n")
		if err != nil {
			return nil, err
		}
		frames = append(frames, done)
	}
	return frames, nil
}

func (st *StreamState) account(frame string) (string, error) {
	st.outBytes += len(frame)
	if st.outBytes > LimitTotal {
		return "", protoErr("输出超过大小限制")
	}
	return frame, nil
}

// encode 把一帧信封序列化成 "data: {...}\n\n"。
func (st *StreamState) encode(v any) ([]string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, protoErr("chunk 序列化失败")
	}
	frame, err := st.account("data: " + string(b) + "\n\n")
	if err != nil {
		return nil, err
	}
	return []string{frame}, nil
}

// ---------------------------------------------------------------------------
// 事件分发（对应 Java 的 Decoder.accept）
// ---------------------------------------------------------------------------

var eventCamelRe = regexp.MustCompile(`([a-z0-9])([A-Z])`)
var eventJunkRe = regexp.MustCompile(`[^A-Za-z0-9]+`)

// normalizeEvent 把上游任意写法的事件名归一化：camelCase → snake_case、
// 非字母数字 → '_'、小写、去首尾 '_'。
func normalizeEvent(e string) string {
	e = eventCamelRe.ReplaceAllString(e, "${1}_${2}")
	e = eventJunkRe.ReplaceAllString(e, "_")
	e = strings.ToLower(e)
	return strings.Trim(e, "_")
}

func (st *StreamState) accept(event, data string) ([]string, error) {
	if st.Terminal || st.Ended {
		return nil, protoErr("event after terminal")
	}
	st.size += len(data)
	if len(data) > LimitFrame || st.size > 8*LimitFrame {
		return nil, protoErr("stream buffer limit")
	}

	e := normalizeEvent(event)

	// 上游用一帧 [DONE] 直接宣布结束（此时不补 usage）。
	if data == "[DONE]" {
		frames, err := st.complete()
		if err != nil {
			return nil, err
		}
		st.Terminal = true
		return frames, nil
	}

	// 辅助事件的载荷是裸字符串，直接丢；是对象则往下走，按有无语义字段判断。
	if _, aux := auxiliaryEvents[e]; aux && !strings.HasPrefix(strings.TrimSpace(data), "{") {
		return nil, nil
	}

	d, err := parseObject(strings.TrimSpace(data))
	if err != nil {
		return nil, err
	}

	// 错误判定：顶到上一层就看这四件事。
	if up := upstreamErrorIf(e, d); up != nil {
		return nil, up
	}

	// 纯用量事件。
	if e == "token_usage" {
		if u, ok := d["usage"].(map[string]any); ok {
			st.putUsage(u)
		} else {
			st.putUsage(d)
		}
		return nil, nil
	}

	// 终止事件。
	switch e {
	case "done", "response_done", "stream_done", "stream_finish":
		if u, ok := d["usage"].(map[string]any); ok {
			st.putUsage(u)
		}
		st.finish(d)
		frames, err := st.complete()
		if err != nil {
			return nil, err
		}
		st.Terminal = true
		return frames, nil
	case "extra_info":
		return nil, nil
	}

	// 只认正文类事件；别的事件里若带语义字段就报错，否则当辅助事件丢掉。
	if _, ok := textEvents[e]; !ok {
		if hasSemantic(d) {
			return nil, protoErr("unsupported semantic event: " + e)
		}
		if _, aux := auxiliaryEvents[e]; !aux {
			return nil, protoErr("unsupported semantic event: " + e)
		}
		return nil, nil
	}

	var out []string
	if err := st.consumeText(d, e, &out); err != nil {
		return nil, err
	}
	st.finish(d)
	return out, nil
}

// consumeText 处理一个正文类事件：思考、正文、原生 tool_calls、嵌套 usage。
func (st *StreamState) consumeText(d map[string]any, e string, out *[]string) error {
	deltaObj, _ := d["delta"].(map[string]any)

	// ---- 思考（reasoning）----
	// 顶层 reasoning_content / reasoning 在 Trae 里往往是**累积**字符串，
	// 而 delta 里的则是增量。所以顶层走「去重/取后缀」，delta 里直接用。
	r, rIsDelta := firstString(d, "reasoning_content", "reasoning")
	deltaReasoning := !rIsDelta
	if deltaReasoning {
		r, _ = firstString(deltaObj, "reasoning_content", "reasoning")
	}
	if r != "" {
		add := r
		if !deltaReasoning {
			// 判重/取后缀按「字符数」算（与 Java 的 String.length 对齐，不是字节数）。
			if r == st.lastThought && utf8.RuneCountInString(r) >= 8 {
				add = ""
			} else if utf8.RuneCountInString(st.thought) >= 16 && strings.HasPrefix(r, st.thought) {
				add = r[len(st.thought):]
			} else if utf8.RuneCountInString(st.thought) >= 16 && utf8.RuneCountInString(r) >= 16 && strings.HasSuffix(st.thought, r) {
				add = ""
			}
			st.lastThought = r
		}
		st.thought += add
		if len(st.thought) > LimitFrame {
			return protoErr("reasoning buffer limit")
		}
		if add != "" {
			st.reasoningOut.WriteString(add)
			frames, err := st.emitChunk(chunkDelta{ReasoningContent: add}, nil)
			if err != nil {
				return err
			}
			*out = append(*out, frames...)
		}
	}

	// ---- 正文 ----
	// 取值优先级：response → content → delta.content → delta（字符串形态）。
	var t any
	present := false
	if v, ok := d["response"]; ok {
		t, present = v, true
	} else if v, ok := d["content"]; ok {
		t, present = v, true
	} else if deltaObj != nil {
		if v, ok := deltaObj["content"]; ok {
			t, present = v, true
		} else if v, ok := d["delta"]; ok {
			t, present = v, true
		}
	} else if v, ok := d["delta"]; ok {
		t, present = v, true
	}
	if present && t != nil {
		switch v := t.(type) {
		case string:
			st.text += v
			if len(st.text) > LimitFrame {
				return protoErr("text buffer limit")
			}
			if err := st.drainText(out, false); err != nil {
				return err
			}
		case map[string]any:
			// 对象形态的 response/content 不带正文，容忍并忽略。
		default:
			return protoErr("unsupported output content")
		}
	}

	// ---- 原生 tool_calls ----
	if a, ok := d["tool_calls"].([]any); ok {
		for _, item := range a {
			tc, ok := item.(map[string]any)
			if !ok {
				return protoErr("invalid tool call")
			}
			if err := st.nativeCall(tc); err != nil {
				return err
			}
		}
	} else if deltaObj != nil {
		if a, ok := deltaObj["tool_calls"].([]any); ok {
			for _, item := range a {
				tc, ok := item.(map[string]any)
				if !ok {
					return protoErr("invalid tool call")
				}
				if err := st.nativeCall(tc); err != nil {
					return err
				}
			}
		}
	}

	// ---- 夹在事件里的 usage ----
	if u, ok := d["usage"].(map[string]any); ok {
		st.putUsage(u)
	}
	return nil
}

// nativeCall 处理上游原生 tool_calls 的一个分片：按 id 或 index 归并，
// arguments 是「增量拼接」而不是覆盖（同一 index 的后续分片只带半截 JSON）。
func (st *StreamState) nativeCall(t map[string]any) error {
	id, _ := t["id"].(string)
	if id == "" {
		id, _ = t["tool_call_id"].(string)
	}

	var idx *int
	if raw, ok := t["index"]; ok && raw != nil {
		n, ok := integralValue(raw)
		if !ok || n < 0 || n > math.MaxInt32 {
			return protoErr("invalid tool index")
		}
		i := int(n)
		idx = &i
	}

	f, ok := t["function"].(map[string]any)
	if !ok {
		f, ok = t["function_call"].(map[string]any)
	}
	if !ok {
		f = t
	}
	name, _ := f["name"].(string)
	if name == "" {
		name, _ = t["tool_name"].(string)
	}

	var argRaw any
	hasArg := false
	if v, ok := f["arguments"]; ok {
		argRaw, hasArg = v, true
	} else if v, ok := f["args"]; ok {
		argRaw, hasArg = v, true
	} else if v, ok := t["params"]; ok {
		argRaw, hasArg = v, true
	}
	args := ""
	if hasArg && argRaw != nil {
		if s, ok := argRaw.(string); ok {
			args = s
		} else {
			args = mustJSON(argRaw)
		}
	}

	// 先按 id 或 index 找已存在的调用。
	var found *toolCall
	for _, c := range st.calls {
		match := (id != "" && c.ID == id) || (idx != nil && c.Index != nil && *c.Index == *idx)
		if !match {
			continue
		}
		if found != nil && found != c {
			return protoErr("conflicting tool identity")
		}
		found = c
	}
	if found == nil {
		if id == "" && idx == nil {
			return protoErr("tool fragment has no identity")
		}
		if len(st.calls) >= MaxCalls {
			return protoErr("too many tools")
		}
		found = &toolCall{}
		st.calls = append(st.calls, found)
	}
	if id != "" {
		if found.ID != "" && found.ID != id {
			return protoErr("conflicting tool id")
		}
		found.ID = id
	}
	if idx != nil {
		if found.Index != nil && *found.Index != *idx {
			return protoErr("conflicting tool index")
		}
		found.Index = idx
	}
	// 名字冲突：已声明的完整名字不允许被另一个前缀不同的名字替换。
	if found.Name != "" && name != "" && st.hasName(found.Name) && found.Name != name && !strings.HasPrefix(name, found.Name) {
		return protoErr("conflicting tool name")
	}
	found.Name = mergeName(found.Name, name)
	merged, err := mergeArguments(found.Args, args)
	if err != nil {
		return err
	}
	found.Args = merged
	if len(found.Name)+len(found.Args) > LimitFrame {
		return protoErr("tool buffer limit")
	}
	return nil
}

// mergeName 合并名字分片：完全包含关系直接取长的，否则拼接（上游可能拆字输出）。
func mergeName(old, next string) string {
	if next == "" || old == next || strings.HasPrefix(old, next) {
		return old
	}
	if strings.HasPrefix(next, old) {
		return next
	}
	return old + next
}

// mergeArguments 合并 arguments 分片：
//   - 前缀关系取长的（上游有时重发完整快照）
//   - 已经是一个完整 JSON 对象时，再来的片段只能是它的重复；否则是错误
func mergeArguments(old, next string) (string, error) {
	if old == "" {
		return next, nil
	}
	if next == "" || old == next {
		return old, nil
	}
	if strings.HasPrefix(next, old) {
		return next, nil
	}
	if isCompleteObject(old) {
		if isCompleteObject(next) {
			return "", protoErr("conflicting complete argument snapshots")
		}
		return "", protoErr("arguments after complete object")
	}
	return old + next, nil
}

func isCompleteObject(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	return strictIsObject(s)
}

// finish 读取终止原因：长度类 → length，敏感内容 → content_filter。
func (st *StreamState) finish(d map[string]any) {
	f := ""
	for _, k := range []string{"finish_reason", "stop_reason", "finishReason"} {
		if v, ok := d[k]; ok && v != nil {
			if s, ok := v.(string); ok {
				f = s
			}
			break
		}
	}
	f = strings.ToLower(f)
	switch f {
	case "length", "max_tokens", "max_output_tokens", "max_length":
		st.FinishReason = "length"
	case "content_filter", "sensitive":
		st.FinishReason = "content_filter"
	}
}

// ---------------------------------------------------------------------------
// 正文吐字机：把缓冲文本切成「正文」与「工具调用」
// ---------------------------------------------------------------------------

// markers 是文本形式工具调用的起始标记。任何一个出现都意味着后续可能不是正文，
// 因此在出现（或可能正在出现）的位置先压住不发。
var markers = []string{
	"<tool_call", "<toolcall", "</tool_call", "</toolcall",
	"<function_calls", "</function_calls",
	"<antml:function_calls", "</antml:function_calls",
	"<invoke", "<antml:invoke", "</antml:invoke",
	"<function=",
	"<｜DSML｜", "<|DSML|", "</｜DSML｜", "</|DSML|",
}

var xmlMarkers = map[string]struct{}{
	"<function_calls":       {},
	"<antml:function_calls": {},
	"<invoke":               {},
	"<antml:invoke":         {},
	"<function=":            {},
}

// closers 是工具块收尾标记（出现在正文位置时原样当正文吐出）。
var closers = []string{
	"</antml:function_calls>", "</function_calls>", "</antml:invoke>", "</invoke>",
}

// leftovers 是工具调用解析完后紧跟的多余收尾标记，需要静默吃掉。
var leftovers = []string{
	"</tool_call>", "</toolcall>", "</invoke>", "</antml:invoke>",
	"</function>", "</function_calls>", "</antml:function_calls>",
}

func isXMLMarker(marker string) bool { _, ok := xmlMarkers[marker]; return ok }

// drainText 把缓冲文本里已经可以确定的部分吐出去。last=false 表示流还没结束，
// 走到「可能还有后续」的边界就停下等下一帧；last=true 表示必须给出结论。
func (st *StreamState) drainText(out *[]string, last bool) error {
	// 刚解析完一个工具调用：紧跟的空白与收尾标记都是它的残渣。
	if st.afterCall {
		st.skipLeftover()
		if len(st.text) == 0 {
			return nil
		}
	}

	for len(st.text) > 0 {
		s := st.text
		pos := -1
		var marker string
		for _, m := range markers {
			x := strings.Index(s, m)
			if x >= 0 && (pos < 0 || x < pos) {
				pos, marker = x, m
			}
		}

		// 正文在前：安全吐出。
		if pos > 0 {
			if err := st.emit(out, s[:pos]); err != nil {
				return err
			}
			continue
		}

		if pos == 0 {
			// 单独的收尾标记：若刚结束一个调用，是残渣吃掉；否则当正文。
			if closer := closerAt(s); closer != "" {
				if st.afterCall {
					st.text = s[len(closer):]
					continue
				}
				if err := st.emit(out, closer); err != nil {
					return err
				}
				continue
			}
			// 残渣标记还没写全，先压住，跟着调用一起丢掉。
			if st.afterCall && leftoverPrefix(s) {
				if !last {
					return nil
				}
				return protoErr("truncated textual tool marker")
			}
			if isXMLMarker(marker) {
				opener := openerOf(s)
				quoted := st.quoted.quoted()
				// 标记还没写全：等下一帧；行内代码/围栏里则是正文。
				if xmlPartial(s) {
					if !last {
						return nil
					}
					if quoted {
						if err := st.emit(out, opener); err != nil {
							return err
						}
						continue
					}
					return protoErr("truncated textual tool marker")
				}
				if quoted {
					if err := st.emit(out, opener); err != nil {
						return err
					}
					continue
				}
				x, err := st.xmlCall(s, last)
				if err != nil {
					return err
				}
				if x.wait {
					return nil
				}
				if x.isText {
					if err := st.emit(out, opener); err != nil {
						return err
					}
					continue
				}
				if err := st.addXMLCall(x.name, x.args); err != nil {
					return err
				}
				st.text = s[len(s)-len(x.rest):]
				st.afterCall = true
				st.skipLeftover()
				continue
			}
			// 收尾标记写在正文里 / 流在收尾标记里结束：当正文，不报错。
			if strings.HasPrefix(marker, "</") {
				if !last && closerPrefix(s) {
					return nil
				}
				if st.quoted.quoted() {
					if err := st.emit(out, openerOf(s)); err != nil {
						return err
					}
					continue
				}
				return protoErr("truncated textual tool marker")
			}
			if marker != "<tool_call" {
				// <toolcall 是同一块的另一种拼法；代码里则当正文。
				if (strings.HasPrefix(marker, "<tool_call") || strings.HasPrefix(marker, "<toolcall")) && st.quoted.quoted() {
					if err := st.emit(out, openerOf(s)); err != nil {
						return err
					}
					continue
				}
				return protoErr("unsupported textual tool marker")
			}

			// 通用 <tool_call> 块。
			const open = "<tool_call>"
			if len(s) < len(open) {
				if !last {
					return nil
				}
				if st.quoted.quoted() {
					if err := st.emit(out, openerOf(s)); err != nil {
						return err
					}
					continue
				}
				return protoErr("truncated textual tool marker")
			}
			if !strings.HasPrefix(s, open) {
				if st.quoted.quoted() {
					if err := st.emit(out, openerOf(s)); err != nil {
						return err
					}
					continue
				}
				return protoErr("unsupported textual tool marker")
			}
			// <tool_call> 里紧跟 '<' 说明里面是 Qwen/Anthropic 标签：丢掉外层开标签继续读。
			if strings.HasPrefix(strings.TrimSpace(s[len(open):]), "<") {
				st.text = s[len(open):]
				st.afterCall = true
				st.skipLeftover()
				continue
			}
			q := strings.Index(s[len(open):], "</tool_call>")
			if q < 0 {
				if last {
					return protoErr("truncated textual tool call")
				}
				return nil
			}
			body := strings.TrimSpace(s[len(open) : len(open)+q])
			if err := st.parseTextCall(body); err != nil {
				return err
			}
			st.text = s[len(open)+q+len("</tool_call>"):]
			continue
		}

		// 没有标记：可能是某个标记的前半截，保住这段尾巴。
		keep := 0
		for _, m := range markers {
			for n := 1; n < len(m) && n <= len(s); n++ {
				if strings.HasSuffix(s, m[:n]) && n > keep {
					keep = n
				}
			}
		}
		// 流结束在单个 '<' 上：当正文（"a < b"）；是标记前缀且长度 >1 则判定被截断。
		if last && keep > 1 {
			return protoErr("truncated textual tool marker")
		}
		shown := len(s)
		if !(last && keep == 1) {
			shown = len(s) - keep
		}
		if shown > 0 {
			if err := st.emit(out, s[:shown]); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

// emit 吐一段正文，并记住它把引号/围栏状态带到了哪里。
func (st *StreamState) emit(out *[]string, s string) error {
	if s == "" {
		return nil
	}
	frames, err := st.emitChunk(chunkDelta{Content: s}, nil)
	if err != nil {
		return err
	}
	*out = append(*out, frames...)
	st.quoted.feed(s)
	st.afterCall = false
	st.text = st.text[len(s):]
	st.contentOut.WriteString(s)
	return nil
}

// skipLeftover 吃掉工具调用前后的空白与多余收尾标记。
func (st *StreamState) skipLeftover() {
	for {
		s := st.text
		i := 0
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		n := leftoverTagAt(s, i)
		if n <= 0 {
			if i > 0 {
				st.text = s[i:]
			}
			return
		}
		st.text = s[i+n:]
	}
}

func leftoverTagAt(s string, i int) int {
	if i > len(s) {
		return 0
	}
	for _, t := range leftovers {
		if strings.HasPrefix(s[i:], t) {
			return len(t)
		}
	}
	return 0
}

func leftoverPrefix(s string) bool {
	for _, t := range leftovers {
		if strings.HasPrefix(t, s) {
			return true
		}
	}
	return false
}

func closerAt(s string) string {
	for _, c := range closers {
		if strings.HasPrefix(s, c) {
			return c
		}
	}
	return ""
}

func closerPrefix(s string) bool {
	for _, c := range closers {
		if strings.HasPrefix(c, s) {
			return true
		}
	}
	return false
}

// openerOf 取出「这个标记的开标签」：到第一个 '>' 或下一个 '<' 为止。
func openerOf(s string) string {
	gt := strings.IndexByte(s, '>')
	lt := strings.IndexByte(s[1:], '<')
	if lt >= 0 {
		lt++
	}
	if gt >= 0 && (lt < 0 || gt < lt) {
		return s[:gt+1]
	}
	if lt < 0 {
		return s
	}
	return s[:lt]
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\r' || c == '\t' }

// ---------------------------------------------------------------------------
// XML 形式（Qwen / Anthropic）工具调用
// ---------------------------------------------------------------------------

var (
	invokeOpenRe    = regexp.MustCompile(`^<(?:antml:)?invoke\s+name\s*=\s*"([^"<>]+)"\s*>`)
	functionOpenRe  = regexp.MustCompile(`^<function=([^\s<>]+?)\s*>`)
	invokeCloseRe   = regexp.MustCompile(`</(?:antml:)?invoke\s*>`)
	functionCloseRe = regexp.MustCompile(`</function\s*>`)
	nextXMLRe       = regexp.MustCompile(`<(?:antml:)?invoke\s+name\s*=|<function=`)
	waitXMLRe       = regexp.MustCompile(`^<(?:antml:)?(?:invoke\s|function=)[^<>]{0,300}$`)
	paramOpenRe     = regexp.MustCompile(`<parameter(?:\s+name\s*=\s*"([^"]+)"|=([^\s>]+))\s*>`)
	parameterAnyRe  = regexp.MustCompile(`<parameter\b`)
	antmlParamRe    = regexp.MustCompile(`<(/?)(?:antml:)?parameter\b`)
	invokePartialRe = regexp.MustCompile(`^<(?:antml:)?invoke\s[^<>]*$`)
	// GLM 风格的 <tool_call>name<arg_key>k</arg_key><arg_value>v</arg_value></tool_call>
	glmCallRe = regexp.MustCompile(`(?s)^([^\s<]+)\s*((?:<arg_key>.*?</arg_value>\s*)+)$`)
	glmKVRe   = regexp.MustCompile(`(?s)<arg_key>(.*?)</arg_key>\s*<arg_value>(.*?)</arg_value>`)
)

// xmlPartial 判断这个标签是不是「还没写全」。
func xmlPartial(s string) bool {
	for _, t := range []string{"<function_calls>", "<antml:function_calls>", "<function="} {
		if strings.HasPrefix(t, s) {
			return true
		}
	}
	return invokePartialRe.MatchString(s) || s == "<invoke" || s == "<antml:invoke"
}

// xmlStarts 判断 s 是不是 <invoke ... / <antml:invoke ... / <function= 的开头。
func xmlStarts(s string) bool {
	return strings.HasPrefix("<invoke ", s) ||
		strings.HasPrefix("<antml:invoke ", s) ||
		strings.HasPrefix("<function=", s)
}

type xmlResult struct {
	wait   bool // 还没写全，等下一帧
	isText bool // 不是工具调用（名字没声明过 / 不是标签），当正文
	name   string
	args   string
	rest   string // 解析之后剩余的文本
}

// xmlCall 读 Anthropic 的 <function_calls> 包装或 Qwen 的 <function=…>。
func (st *StreamState) xmlCall(s string, last bool) (xmlResult, error) {
	w := 0
	if strings.HasPrefix(s, "<function_calls>") {
		w = len("<function_calls>")
	} else if strings.HasPrefix(s, "<antml:function_calls>") {
		w = len("<antml:function_calls>")
	}
	if w > 0 {
		rest := s[w:]
		i := 0
		for i < len(rest) && isSpace(rest[i]) {
			i++
		}
		rest = rest[i:]
		if !last && (rest == "" || xmlStarts(rest)) {
			return xmlResult{wait: true}, nil
		}
		return st.xmlInvoke(rest, last)
	}
	return st.xmlInvoke(s, last)
}

// xmlInvoke 读一个 invoke：声明过的工具就是调用，没声明过就当正文，写一半就等。
func (st *StreamState) xmlInvoke(s string, last bool) (xmlResult, error) {
	loc := invokeOpenRe.FindStringSubmatchIndex(s)
	closeRe := invokeCloseRe
	if loc == nil {
		loc = functionOpenRe.FindStringSubmatchIndex(s)
		closeRe = functionCloseRe
	}
	if loc == nil {
		if !last && (xmlStarts(s) || waitXMLRe.MatchString(s)) {
			return xmlResult{wait: true}, nil
		}
		return xmlResult{isText: true}, nil
	}

	declared := st.declaredName(strings.TrimSpace(s[loc[2]:loc[3]]))
	if declared == "" {
		return xmlResult{isText: true}, nil
	}

	body := s[loc[1]:]
	stop, closeEnd := -1, -1
	if c := closeRe.FindStringIndex(body); c != nil {
		stop, closeEnd = c[0], c[1]
	}
	if n := nextXMLRe.FindStringIndex(body); n != nil && (stop < 0 || n[0] < stop) {
		stop, closeEnd = n[0], -1
	}
	if stop < 0 {
		if !last {
			return xmlResult{wait: true}, nil
		}
		stop = len(body)
	}
	whole := closeEnd >= 0

	region := body[:stop]
	// <antml:parameter ...> 与 <parameter ...> 同样处理。
	// 注意：Go 的 $1parameter 会被当成变量名「1parameter」，必须写成 ${1}。
	region = antmlParamRe.ReplaceAllString(region, "<${1}parameter")

	args := map[string]any{}
	closed, err := st.paramArgs(region, declared, args)
	if err != nil {
		return xmlResult{}, err
	}
	if !whole && !closed {
		if !parameterAnyRe.MatchString(region) {
			return xmlResult{isText: true}, nil
		}
		return xmlResult{}, protoErr("textual tool call cut short")
	}

	argsText, err := json.Marshal(args)
	if err != nil {
		return xmlResult{}, protoErr("tool arguments serialization failed")
	}
	rest := body[stop:]
	if whole {
		rest = body[closeEnd:]
	}
	return xmlResult{name: declared, args: string(argsText), rest: rest}, nil
}

// declaredName 把模型写出的工具名映射回请求里声明的那个名字
// （Read → read、list-files → list_files），匹配不上返回空。
func (st *StreamState) declaredName(candidate string) string {
	if candidate == "" || len(st.names) == 0 {
		return ""
	}
	if _, ok := st.names[candidate]; ok {
		return candidate
	}
	want := normalizedName(candidate)
	if want == "" {
		return ""
	}
	found := ""
	for n := range st.names {
		if want == normalizedName(n) {
			if found != "" {
				// 两个声明的工具撞成同一个规范名，无法判定，宁可不认。
				return ""
			}
			found = n
		}
	}
	return found
}

var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]`)

func normalizedName(n string) string {
	return nonAlnumRe.ReplaceAllString(strings.ToLower(n), "")
}

// paramArgs 解析 <parameter name="k">v</parameter>：
// 声明 schema 里是 string 的按原文本取值，其余的按 JSON 解析（解析不了就当字符串）。
func (st *StreamState) paramArgs(region, name string, args map[string]any) (bool, error) {
	spans := paramOpenRe.FindAllStringSubmatchIndex(region, -1)
	keys := make([]string, len(spans))
	for i, m := range spans {
		if m[2] >= 0 {
			keys[i] = region[m[2]:m[3]]
		} else if m[4] >= 0 {
			keys[i] = region[m[4]:m[5]]
		}
	}
	closed := len(spans) > 0
	properties := st.props[name]

	for i := range spans {
		start := spans[i][1]
		var end int
		if i+1 < len(spans) {
			end = spans[i+1][0]
		} else {
			end = len(region)
		}
		if end < start {
			end = start
		}
		v := region[start:end]
		if c := strings.Index(v, "</parameter>"); c >= 0 {
			v = v[:c]
		} else {
			closed = false
		}
		v = newlinesOff(v)

		var propType string
		if properties != nil {
			if p, ok := properties[keys[i]].(map[string]any); ok {
				propType, _ = p["type"].(string)
			}
		}
		if propType == "string" {
			args[keys[i]] = v
			continue
		}
		if parsed, ok := jsonValue(strings.TrimSpace(v)); ok {
			args[keys[i]] = parsed
		} else {
			args[keys[i]] = v
		}
	}
	return closed, nil
}

// newlinesOff 去掉值两端各一个换行（模型习惯把参数写成多行）。
func newlinesOff(v string) string {
	if strings.HasPrefix(v, "\r\n") {
		v = v[2:]
	} else if strings.HasPrefix(v, "\n") {
		v = v[1:]
	}
	if strings.HasSuffix(v, "\r\n") {
		v = v[:len(v)-2]
	} else if strings.HasSuffix(v, "\n") {
		v = v[:len(v)-1]
	}
	return v
}

// jsonValue 尝试把字符串解析成一个 JSON 值。
func jsonValue(s string) (any, bool) {
	if s == "" {
		return nil, false
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, false
	}
	return v, true
}

func (st *StreamState) addXMLCall(name, args string) error {
	if len(st.calls) >= MaxCalls {
		return protoErr("too many tools")
	}
	if len(name)+len(args) > LimitFrame {
		return protoErr("tool buffer limit")
	}
	st.calls = append(st.calls, &toolCall{ID: newCallID(), Name: name, Args: args})
	return nil
}

// parseTextCall 解析 <tool_call> 里的内容：
//   - JSON 形式：{"name":..,"arguments":..}
//   - GLM 形式：name<arg_key>k</arg_key><arg_value>v</arg_value>…
func (st *StreamState) parseTextCall(body string) error {
	var name, argsText string

	if strings.HasPrefix(body, "{") {
		call, err := parseObject(body)
		if err != nil {
			return err
		}
		nv, ok := call["name"]
		if !ok {
			return protoErr("missing name")
		}
		if name, ok = nv.(string); !ok || strings.TrimSpace(name) == "" {
			return protoErr("missing name")
		}
		av, ok := call["arguments"]
		if !ok || av == nil {
			return protoErr("missing textual tool arguments")
		}
		if s, ok := av.(string); ok {
			argsText = s
		} else {
			argsText = mustJSON(av)
		}
	} else {
		m := glmCallRe.FindStringSubmatch(body)
		if m == nil {
			return protoErr("unsupported textual tool call")
		}
		name = m[1]
		rest := m[2]
		args := map[string]any{}
		end := 0
		for _, kv := range glmKVRe.FindAllStringSubmatchIndex(rest, -1) {
			if strings.TrimSpace(rest[end:kv[0]]) != "" {
				return protoErr("malformed GLM tool call")
			}
			key := rest[kv[2]:kv[3]]
			if _, dup := args[key]; dup {
				return protoErr("malformed GLM tool call")
			}
			args[key] = rest[kv[4]:kv[5]]
			end = kv[1]
		}
		if strings.TrimSpace(rest[end:]) != "" {
			return protoErr("malformed GLM tool call")
		}
		argsText = mustJSON(args)
	}

	if !strictIsObject(argsText) {
		return protoErr("arguments must be a JSON object")
	}
	if len(st.calls) >= MaxCalls {
		return protoErr("too many tools")
	}
	// 文本语法不带 ID，这里自己造一个。
	st.calls = append(st.calls, &toolCall{ID: newCallID(), Name: name, Args: argsText})
	return nil
}

// ---------------------------------------------------------------------------
// 收尾：把缓冲的工具调用整体吐出
// ---------------------------------------------------------------------------

// complete 结束一条回复：先冲掉压着的正文，再把工具调用一次性输出（中途不改主意）。
func (st *StreamState) complete() ([]string, error) {
	var out []string
	if err := st.drainText(&out, true); err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	var arr []toolCallDelta
	for i, c := range st.calls {
		if c.ID == "" || strings.TrimSpace(c.Name) == "" || c.Args == "" {
			return nil, protoErr("incomplete native tool call")
		}
		if _, dup := seen[c.ID]; dup {
			return nil, protoErr("duplicate tool call id")
		}
		seen[c.ID] = struct{}{}
		// 声明过工具时才校验名字；没声明（例如只有 model 的 ConvertStream）就放行。
		if len(st.names) > 0 && !st.hasName(c.Name) {
			return nil, protoErr("unknown tool name")
		}
		if !strictIsObject(c.Args) {
			return nil, protoErr("arguments must be a JSON object")
		}
		arr = append(arr, toolCallDelta{
			Index:    i,
			ID:       c.ID,
			Type:     "function",
			Name:     c.Name,
			ArgsText: c.Args,
		})
	}

	if len(arr) > 0 {
		frames, err := st.emitChunk(chunkDelta{ToolCalls: arr}, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, frames...)
		st.FinishReason = "tool_calls"
	} else if st.FinishReason == "" {
		st.FinishReason = "stop"
	}

	final, err := st.emitChunk(chunkDelta{}, &st.FinishReason)
	if err != nil {
		return nil, err
	}
	out = append(out, final...)
	st.Ended = true
	return out, nil
}

// emitChunk 生成一帧 chat.completion.chunk。
func (st *StreamState) emitChunk(delta chunkDelta, finish *string) ([]string, error) {
	return st.encode(chunkEnvelope{
		ID:      st.ChunkID,
		Object:  "chat.completion.chunk",
		Created: st.Created,
		Model:   st.Model,
		Choices: []chunkChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finish,
		}},
	})
}

// ---------------------------------------------------------------------------
// 用量
// ---------------------------------------------------------------------------

func (st *StreamState) putUsage(u map[string]any) {
	if u == nil {
		return
	}
	p, okP := countOf(u, "prompt_tokens", "input_tokens", "promptTokens", "inputTokens")
	c, okC := countOf(u, "completion_tokens", "output_tokens", "completionTokens", "outputTokens")
	total, okT := countOf(u, "total_tokens", "totalTokens")
	if !okP && !okC && !okT {
		return
	}
	if p == 0 && c == 0 {
		return
	}
	if total == 0 {
		total = p + c
	}
	v := map[string]any{
		"prompt_tokens":     p,
		"completion_tokens": c,
		"total_tokens":      total,
	}

	pd, _ := u["prompt_tokens_details"].(map[string]any)
	id, _ := u["input_tokens_details"].(map[string]any)
	standard := maxInt64(
		maxInt64(countAnyway(pd, "cached_tokens"), countAnyway(id, "cached_tokens")),
		maxInt64(countAnyway(u, "cached_tokens"), countAnyway(u, "prompt_cache_hit_tokens")),
	)
	writeAway := countAnyway(pd, "cache_write_tokens")
	readNative := countAnyOf(u, "cache_read_input_tokens", "cacheReadInputTokens")
	writeNative := countAnyOf(u, "cache_creation_input_tokens", "cacheCreationInputTokens")

	// 全 0 的缓存上报保守地当作「未知」，不能当成命中率 0。
	if standard > 0 || writeAway > 0 {
		detail := map[string]any{}
		if standard > 0 {
			detail["cached_tokens"] = standard
		}
		if writeAway > 0 {
			detail["cache_write_tokens"] = writeAway
		}
		v["prompt_tokens_details"] = detail
	} else if readNative > 0 || writeNative > 0 {
		if readNative > 0 {
			v["cache_read_input_tokens"] = readNative
		}
		if writeNative > 0 {
			v["cache_creation_input_tokens"] = writeNative
		}
	} else {
		v["magpie_cache_unknown"] = true
	}

	cd, _ := u["completion_tokens_details"].(map[string]any)
	reasoning := maxInt64(countAnyway(u, "reasoning_tokens"), countAnyway(cd, "reasoning_tokens"))
	if reasoning > 0 {
		v["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	st.Usage = v
}

// countOf 取计数：找到任一 key 就返回其值（找不到返回 0,false），值非法则不认。
func countOf(o map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		v, ok := o[k]
		if !ok || v == nil {
			continue
		}
		n, ok := integralValue(v)
		if !ok || n < 0 || n > 9007199254740991 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func countAnyway(o map[string]any, keys ...string) int64 {
	n, _ := countOf(o, keys...)
	return n
}

func countAnyOf(o map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := o[k]; ok && v != nil {
			if n, ok := integralValue(v); ok && n > 0 {
				return n
			}
		}
	}
	return 0
}

// integralValue 把 JSON 数字转成整数，非整数/非数字返回 false。
func integralValue(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil || f != math.Trunc(f) {
			return 0, false
		}
		return int64(f), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// 上游错误映射
// ---------------------------------------------------------------------------

var quotaRe = regexp.MustCompile(`\b(quota|rate|exceed(?:ed|s|ing)?)\b`)
var codeRe = regexp.MustCompile(`^[0-9]{1,32}$`)

// upstreamErrorIf 判断这个事件是不是上游报错，是则返回映射后的错误。
func upstreamErrorIf(e string, d map[string]any) *UpstreamError {
	if e == "error" {
		return upstreamErrorFrom(d)
	}
	if _, ok := d["error"]; ok {
		return upstreamErrorFrom(d)
	}
	if meta, ok := d["ResponseMetadata"].(map[string]any); ok {
		if _, ok := meta["Error"]; ok {
			return upstreamErrorFrom(d)
		}
	}
	if code, ok := integralValue(d["code"]); ok && code != 0 {
		_, hasMsg := d["message"]
		_, hasMsg2 := d["msg"]
		_, hasResp := d["response"]
		_, hasContent := d["content"]
		if (hasMsg || hasMsg2) && !hasResp && !hasContent {
			return upstreamErrorFrom(d)
		}
	}
	return nil
}

func upstreamErrorFrom(body map[string]any) *UpstreamError {
	detail := body
	if errObj, ok := body["error"].(map[string]any); ok {
		detail = errObj
	} else if meta, ok := body["ResponseMetadata"].(map[string]any); ok {
		if metaErr, ok := meta["Error"].(map[string]any); ok {
			detail = metaErr
		}
	}

	code := ""
	for _, k := range []string{"code", "Code"} {
		if v, ok := detail[k]; ok && v != nil {
			code = valueText(v)
			break
		}
	}
	if code == "" && len(detail) > 0 {
		if v, ok := body["code"]; ok && v != nil {
			if _, same := detail["code"]; !same {
				code = valueText(v)
			}
		}
	}
	if !codeRe.MatchString(code) {
		code = "unknown"
	}

	message := ""
	for _, src := range []map[string]any{detail, body} {
		for _, k := range []string{"message", "msg", "Message"} {
			if v, ok := src[k].(string); ok && v != "" {
				message = v
				break
			}
		}
		if message != "" {
			break
		}
	}

	lower := strings.ToLower(message)
	status := 502
	safe := "Trae 上游返回错误"
	switch {
	case code == "1001" || code == "401":
		status, safe = 401, "Trae 登录已失效，请重新登录"
	case code == "4008" || code == "1005" || quotaRe.MatchString(lower):
		status, safe = 429, "Trae 请求频率或额度已达限制，请稍后重试"
	case code == "4001" || code == "4023":
		status, safe = 400, "Trae 不支持请求的功能"
	}
	return &UpstreamError{Code: code, Status: status, Message: safe}
}

// hasSemantic 判断载荷里有没有协议认得的东西（用来区分「辅助事件」与「不认识的事件」）。
func hasSemantic(d map[string]any) bool {
	for _, k := range []string{
		"response", "content", "delta", "tool_calls", "toolcalls",
		"error", "reasoning", "reasoning_content", "usage",
		"finish_reason", "stop_reason",
	} {
		if _, ok := d[k]; ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// JSON 小工具
// ---------------------------------------------------------------------------

// parseObject 按 JSON 对象解析；空字符串当空对象。非对象、语法错误、尾部多余内容都算错。
func parseObject(data string) (map[string]any, error) {
	t := strings.TrimSpace(data)
	if t == "" {
		return map[string]any{}, nil
	}
	if len(t) > LimitFrame {
		return nil, protoErr("invalid JSON size")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(t), &m); err != nil || m == nil {
		return nil, protoErr("invalid JSON payload")
	}
	return m, nil
}

// strictIsObject 校验一段文本是合法的单个 JSON 对象（工具 arguments 必须如此）。
// 注意：与 Java 版相比这里不检测重复键（encoding/json 后值覆盖前值）。
func strictIsObject(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || len(t) > LimitFrame {
		return false
	}
	if !strings.HasPrefix(t, "{") {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(t), &m); err != nil || m == nil {
		return false
	}
	return true
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// valueText 模拟 Java 的 toString：字符串原样，其余转成 JSON 文本。
func valueText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if n, ok := integralValue(v); ok {
		return fmt.Sprintf("%d", n)
	}
	return mustJSON(v)
}

// firstString 按顺序取第一个非空字符串字段。
func firstString(o map[string]any, keys ...string) (string, bool) {
	if o == nil {
		return "", false
	}
	for _, k := range keys {
		if s, ok := o[k].(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 随机源不可用时退回时间戳，保证 ID 仍然唯一到可用程度。
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func newCallID() string { return "call_" + randomHex(16) }

// ---------------------------------------------------------------------------
// 对外入口 2：整条流聚合
// ---------------------------------------------------------------------------

// ConvertStream 吃完整条上游 SSE，返回一个标准 chat.completion 的 JSON
// （非流式响应）。工具调用按 index 累加后整体给出。
//
// 这里不声明工具：因此 Qwen/Anthropic 形式的文本工具调用会被当作正文
// （与 Java 版「只认声明过的工具」一致）。要解析这三种语法请用
// ConvertStreamWithTools 或自己 NewStreamState + DeclareTools。
func ConvertStream(r io.Reader, model string) ([]byte, error) {
	return convert(r, NewStreamState(model))
}

// ConvertStreamWithTools 与 ConvertStream 相同，但先声明工具集，
// 这样 <tool_call> / <function=…> / <function_calls> 三种语法都会被识别成工具调用。
func ConvertStreamWithTools(r io.Reader, model string, tools []ToolSpec) ([]byte, error) {
	st := NewStreamState(model)
	if err := st.DeclareTools(tools); err != nil {
		return nil, err
	}
	return convert(r, st)
}

func convert(r io.Reader, st *StreamState) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), LimitFrame+1)
	for sc.Scan() {
		if _, err := TransformEvent(sc.Text(), st); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, protoErr("上游读取失败或事件超过大小限制")
	}
	// 流在数据帧中间断掉：没有以空行收尾，说明帧不完整。
	if st.hasData {
		return nil, protoErr("流在结束事件前中断")
	}
	// 没收到终止事件也要把压着的正文冲出来（上游异常断流时尽量给出已有内容）。
	if !st.Ended && !st.Terminal {
		if _, err := st.complete(); err != nil {
			return nil, err
		}
	}
	return st.aggregate()
}

// aggregate 把已累积的正文/思考/工具调用拼成一次性 chat.completion。
func (st *StreamState) aggregate() ([]byte, error) {
	message := map[string]any{"role": "assistant"}

	content := st.contentOut.String()
	if content == "" {
		message["content"] = nil
	} else {
		message["content"] = content
	}
	if rc := st.reasoningOut.String(); rc != "" {
		message["reasoning_content"] = rc
	}
	if len(st.calls) > 0 {
		arr := make([]map[string]any, 0, len(st.calls))
		for _, c := range st.calls {
			arr = append(arr, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": c.Args,
				},
			})
		}
		message["tool_calls"] = arr
	}

	finish := st.FinishReason
	if finish == "" {
		finish = "stop"
	}
	result := map[string]any{
		"id":      st.ChunkID,
		"object":  "chat.completion",
		"created": st.Created,
		"model":   st.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if st.Usage != nil {
		result["usage"] = st.Usage
	}
	return json.Marshal(result)
}
