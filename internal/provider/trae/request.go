// request.go：标准 OpenAI 请求体 → Trae 上游请求体。
//
// Trae 的聊天接口不是 OpenAI 兼容的：端点、字段名、消息结构都不一样。
// 本文件负责「进来的请求」这一侧的翻译，另一侧（响应事件流）在 protocol.go。
//
// # 上游要什么
//
//	POST https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat
//	{
//	  "messages": [{"role":"user","content":[{"type":"text","text":"hi"}]}],
//	  "function": "chat_v3",              // 功能名：chat_v3 / solo_agent / solo_coder …
//	  "config_name": "claude-...",        // 模型配置名
//	  "model": "claude-...",              // 与 config_name 相同
//	  "model_name": "claude-...__max",    // 可选：同一配置下的变体（xxx__dev / xxx__max）
//	  "stream": true,                     // 只支持流式，非流式由网关聚合
//	  "session_id": "<uuid>",             // 多轮会话 id
//	  "request_id": "<uuid>",             // 本条请求 id
//	  "max_tokens": 4096,
//	  "temperature": 0.7,
//	  "tools": [{"type":"function","function":{"name":"..","description":"..","parameters":"<JSON 文本>"}}],
//	  "tool_choice": "auto" | "required" | "<工具名>",
//	  "parallel_tool_calls": true,
//	  "reasoning_effort": "high",
//	  "user_message_context": {"model_info":{"prompt_max_tokens": N}}
//	}
//
// 与 Java 版（TraeProtocol.request）对齐的几处细节：
//   - tools 里的 parameters 要从 JSON 对象改成**字符串**（Trae 就是这么收的）；
//   - tool_choice 是对象时只取里面的工具名，字符串只认 none / auto / required；
//   - tool_choice=none 时不下发 tools，历史里的工具调用改写成正文里的 <tool_call> 文本；
//   - 工具结果在没有原生调用轮次可回时，改写成「Result of 工具名 (call id): 正文」；
//   - 连续的同角色（assistant / user）消息要合并成一条；
//   - Trae 不认的采样字段（top_p / seed / stop / response_format / logprobs …）一律丢弃。
//
// 这一层是网关，比 Java 客户端宽松：能降级处理的输入就降级（丢字段、改写正文），
// 只有语义无法表达（缺 messages、角色不认识、参数不是 JSON 对象）时才报错。
package trae

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const (
	// ChatPath 是 Trae 的聊天端点路径（不在 /v1 下，也不是 /chat/completions）。
	ChatPath = "/api/agent/v3/llm_utils_chat"
	// DefaultFunction 是默认的 Trae 功能名，对应普通对话。
	DefaultFunction = "chat_v3"
	// defaultHost 是没配 baseUrl 时的兜底主机（与 provider.DefaultBaseURL 一致，
	// 但 provider 反过来依赖本包，所以这里另存一份常量，避免循环引用）。
	defaultHost = "https://trae-api-cn.mchost.guru"
	// maxSessionLen 与上游一致：session_id 最长 128 字符。
	maxSessionLen = 128
)

// Options 是 TransformRequestWith 的可选参数。
type Options struct {
	// Function 是 Trae 的功能名，空则用 DefaultFunction。
	Function string
	// ModelName 是模型变体名（形如 xxx__max）；空则按 model 里的 "__" 自动拆。
	ModelName string
	// SessionID 是多轮会话 id；空则每条请求生成一个新的。
	SessionID string
	// PromptMaxTokens 是模型可用于提示的 token 上限；>0 时才下发 user_message_context。
	// Trae 用它算「还剩多少上下文」，不知道就留 0，让上游用模型默认值。
	PromptMaxTokens int
}

// BuildChatURL 把 baseURL 拼成 Trae 的聊天端点。
//
// 容忍几种常见写法：带不带结尾斜杠、误填了 /v1、误填了完整的 /chat/completions、
// 已经写到 /api/agent/v3 或已带完整端点。baseURL 为空时用默认主机。
func BuildChatURL(baseURL string) string {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = defaultHost
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, ChatPath) {
		return base
	}
	// OpenAI 风格的误填：去掉 /v1/chat/completions、/v1/chat、/v1、/chat/completions
	for _, junk := range []string{"/v1/chat/completions", "/chat/completions", "/v1/chat", "/v1"} {
		if strings.HasSuffix(base, junk) {
			base = strings.TrimRight(strings.TrimSuffix(base, junk), "/")
			break
		}
	}
	if base == "" {
		base = defaultHost
	}
	// 已经写到 /api/agent/v3 的，只差最后一段。
	if strings.HasSuffix(base, "/api/agent/v3") {
		return base + "/llm_utils_chat"
	}
	return base + ChatPath
}

// TransformRequest 把标准 OpenAI 请求体转成 Trae 上游请求体。
//
// model 为空时取请求体里的 model 字段。返回的是已经序列化好的 JSON。
func TransformRequest(body []byte, model string) ([]byte, error) {
	return TransformRequestWith(body, model, Options{})
}

// TransformRequestWith 是带选项的版本。
func TransformRequestWith(body []byte, model string, opt Options) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %w", err)
	}
	if strings.TrimSpace(model) == "" {
		model, _ = src["model"].(string)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("缺少 model")
	}

	// 工具与工具选择要先归一化：消息转换需要知道「本次请求有没有原生工具」。
	tools, err := wireTools(src["tools"])
	if err != nil {
		return nil, err
	}
	choice, choiceName, err := wireToolChoice(src["tool_choice"])
	if err != nil {
		return nil, err
	}
	nativeTools := len(tools) > 0 && choice != "none"

	msgs, err := wireMessages(src["messages"], nativeTools)
	if err != nil {
		return nil, err
	}

	// model 写成 xxx__max 时：前半截是配置名，整体是变体名。
	config, modelName := model, strings.TrimSpace(opt.ModelName)
	if modelName == "" {
		if i := strings.Index(model, "__"); i > 0 && i+2 < len(model) {
			config, modelName = model[:i], model
		}
	}

	session := strings.TrimSpace(opt.SessionID)
	if session == "" {
		session = newUUID()
	}
	if len(session) > maxSessionLen {
		return nil, fmt.Errorf("session_id 超过 %d 字符", maxSessionLen)
	}

	fn := strings.TrimSpace(opt.Function)
	if fn == "" {
		fn = DefaultFunction
	}

	out := map[string]any{
		"messages":    msgs,
		"function":    fn,
		"config_name": config,
		"model":       config,
		"stream":      true, // 上游只给流式；客户端要非流式由网关聚合
		"session_id":  session,
		"request_id":  newUUID(),
	}
	if modelName != "" {
		out["model_name"] = modelName
	}

	maxTokens, err := outputLimit(src)
	if err != nil {
		return nil, err
	}
	if maxTokens > 0 {
		out["max_tokens"] = maxTokens
	}

	if t, ok := number(src["temperature"]); ok {
		if t < 0 || t > 2 {
			return nil, fmt.Errorf("temperature 超出 0~2")
		}
		out["temperature"] = t
	}

	if nativeTools {
		out["tools"] = tools
		if choiceName != "" {
			out["tool_choice"] = choiceName
		} else if choice != "" {
			out["tool_choice"] = choice
		}
		if p, ok := src["parallel_tool_calls"].(bool); ok {
			out["parallel_tool_calls"] = p
		}
	}

	if e := reasoningEffort(src); e != "" {
		out["reasoning_effort"] = e
	}

	// user_message_context：只有在知道模型上下文窗口时才带，否则让上游用默认值。
	if opt.PromptMaxTokens > 0 {
		room := opt.PromptMaxTokens - maxTokens
		if room < 0 {
			room = 0
		}
		out["user_message_context"] = map[string]any{
			"model_info": map[string]any{"prompt_max_tokens": room},
		}
	}

	return json.Marshal(out)
}

// ToolSpecs 从标准 OpenAI 请求体里取出工具名与参数，供流式转换识别文本形式的工具调用。
//
// tool_choice=none 视为没声明工具（与请求体里去掉 tools 的行为保持一致）。
func ToolSpecs(body []byte) []ToolSpec {
	var src struct {
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil
	}
	if len(src.Tools) == 0 {
		return nil
	}
	if strings.TrimSpace(string(src.ToolChoice)) == `"none"` {
		return nil
	}
	out := make([]ToolSpec, 0, len(src.Tools))
	for _, t := range src.Tools {
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			continue
		}
		out = append(out, ToolSpec{Name: name, Parameters: t.Function.Parameters})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// wireTools 把 OpenAI 的 tools 转成 Trae 的形状（parameters 变成 JSON 文本）。
func wireTools(raw any) ([]any, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("tools 必须是数组")
	}
	out := make([]any, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools 的元素必须是对象")
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("工具缺少 function")
		}
		name, _ := fn["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("工具缺少 name")
		}
		if seen[name] {
			return nil, fmt.Errorf("工具名重复: %s", name)
		}
		seen[name] = true

		// parameters 上游要字符串形式；缺省给 "{}"。
		params := "{}"
		if v, ok := fn["parameters"]; ok && v != nil {
			if s, ok := v.(string); ok {
				if strings.TrimSpace(s) != "" {
					params = s
				}
			} else {
				b, err := json.Marshal(v)
				if err != nil {
					return nil, fmt.Errorf("工具 %s 的 parameters 序列化失败: %w", name, err)
				}
				params = string(b)
			}
		}
		if !isJSONObject(params) {
			return nil, fmt.Errorf("工具 %s 的 parameters 不是 JSON 对象", name)
		}
		desc, _ := fn["description"].(string)
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": desc,
				"parameters":  params,
			},
		})
	}
	return out, nil
}

// wireToolChoice 归一化 tool_choice：返回字符串形式（none/auto/required）或工具名。
func wireToolChoice(raw any) (choice string, name string, err error) {
	switch v := raw.(type) {
	case nil:
		return "", "", nil
	case string:
		switch v {
		case "auto", "none", "required":
			return v, "", nil
		}
		return "", "", fmt.Errorf("不支持的 tool_choice: %q", v)
	case map[string]any:
		fn, _ := v["function"].(map[string]any)
		n, _ := fn["name"].(string)
		n = strings.TrimSpace(n)
		if n == "" {
			return "", "", fmt.Errorf("tool_choice 缺少工具名")
		}
		return "", n, nil
	}
	return "", "", fmt.Errorf("tool_choice 格式不对")
}

// ---------------------------------------------------------------------------
// 消息
// ---------------------------------------------------------------------------

// historyCall 是一条历史工具调用，两种写法（原生 / 正文文本）共用。
type historyCall struct {
	id   string
	name string
	args string // 原始 arguments 文本（JSON 对象文本）
}

// wireMessages 把 OpenAI 的 messages 转成 Trae 的消息数组。
// native 为 true 表示本次请求声明了原生工具，历史里的调用与结果按 Trae 原生形状发。
func wireMessages(raw any, native bool) ([]any, error) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("messages 必须是非空数组")
	}
	out := make([]map[string]any, 0, len(list))
	// 工具调用 id → 工具名，供「没有原生调用」时把结果写成正文。
	names := make(map[string]string, 4)

	for _, item := range list {
		msg, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages 的元素必须是对象")
		}
		role, _ := msg["role"].(string)
		role = strings.TrimSpace(role)
		if role == "developer" { // OpenAI 的新叫法，Trae 只认 system
			role = "system"
		}
		switch role {
		case "system", "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("不支持的消息角色: %q", role)
		}

		content, err := contentParts(msg["content"])
		if err != nil {
			return nil, err
		}
		next := map[string]any{"role": role, "content": content}

		switch role {
		case "tool":
			id, _ := msg["tool_call_id"].(string)
			id = strings.TrimSpace(id)
			if native {
				if id != "" {
					next["tool_call_id"] = id
				}
				if len(content) == 0 {
					next["content"] = []any{textPart("(no output)")}
				}
			} else {
				// 上游没有对应的原生调用轮次可回，就把结果写成「回答哪个调用」的正文。
				name := names[id]
				if name == "" {
					if s, ok := msg["name"].(string); ok {
						name = strings.TrimSpace(s)
					}
				}
				if name == "" {
					name = "the tool"
				}
				label := id
				if label == "" {
					label = "unknown"
				}
				parts := []any{textPart("Result of " + name + " (call " + label + "):\n" + textOfParts(content))}
				for _, p := range content {
					if pm, ok := p.(map[string]any); ok && pm["type"] == "image_url" {
						parts = append(parts, pm)
					}
				}
				next = map[string]any{"role": "user", "content": parts}
			}

		case "assistant":
			calls, err := parseHistoryCalls(msg["tool_calls"], names)
			if err != nil {
				return nil, err
			}
			if len(calls) > 0 {
				if native {
					next["tool_calls"] = wireCalls(calls)
					if len(content) == 0 {
						next["content"] = nil // Trae 收 content: null
					}
				} else {
					// 没有原生工具可声明时，把历史调用写回正文里的 <tool_call> 文本，
					// 这样模型读到的历史与它自己写出来的格式一致。
					parts := partsOf(next["content"])
					for _, c := range calls {
						parts = append(parts, textPart(callText(c)))
					}
					next["content"] = parts
				}
			}
		}

		out = mergeMessage(out, next)
	}

	// Trae 的 messages 是 []any；这里统一转一次。
	anyOut := make([]any, 0, len(out))
	for _, m := range out {
		anyOut = append(anyOut, m)
	}
	return anyOut, nil
}

// parseHistoryCalls 解析助手消息里的 tool_calls，并把 id→工具名记进 names。
func parseHistoryCalls(raw any, names map[string]string) ([]historyCall, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("tool_calls 必须是数组")
	}
	out := make([]historyCall, 0, len(list))
	for _, item := range list {
		c, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool_calls 的元素必须是对象")
		}
		fn, ok := c["function"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("历史工具调用缺少 function")
		}
		name, _ := fn["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("历史工具调用缺少 name")
		}
		id, _ := c["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			// 少数客户端不回 id：补一个，保证后面的工具结果能对上号。
			id = newCallID()
		}
		names[id] = name
		out = append(out, historyCall{id: id, name: name, args: argumentsText(fn["arguments"])})
	}
	return out, nil
}

// wireCalls 把历史调用写成 Trae 原生的形状：function_call 嵌在条目里。
func wireCalls(calls []historyCall) []any {
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]any{
			"id":   c.id,
			"type": "function",
			"function_call": map[string]any{
				"name":      c.name,
				"arguments": c.args,
			},
		})
	}
	return out
}

// callText 把一条历史调用写成正文里的 <tool_call>{...}</tool_call>。
func callText(c historyCall) string {
	// arguments 是合法 JSON 对象就按对象嵌入，否则原样当字符串（与 Java 版同）。
	var args any = c.args
	if isJSONObject(c.args) {
		args = json.RawMessage(c.args)
	}
	b, err := json.Marshal(map[string]any{"name": c.name, "arguments": args})
	if err != nil {
		return "<tool_call>" + strconvQuote(c.name) + "</tool_call>"
	}
	return "<tool_call>" + string(b) + "</tool_call>"
}

// mergeMessage 把相邻的同角色（assistant / user）消息并成一条。
func mergeMessage(out []map[string]any, next map[string]any) []map[string]any {
	if len(out) == 0 {
		return append(out, next)
	}
	role, _ := next["role"].(string)
	if role != "assistant" && role != "user" {
		return append(out, next)
	}
	last := out[len(out)-1]
	if lrole, _ := last["role"].(string); lrole != role {
		return append(out, next)
	}
	parts := partsOf(last["content"])
	parts = append(parts, partsOf(next["content"])...)
	if len(parts) == 0 {
		last["content"] = nil
	} else {
		last["content"] = parts
	}
	if add, ok := next["tool_calls"].([]any); ok {
		cur, _ := last["tool_calls"].([]any)
		last["tool_calls"] = append(cur, add...)
	}
	return out
}

// contentParts 把 message.content 归一化成 Trae 的 parts 数组。
func contentParts(raw any) ([]any, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(v) == "" {
			// 空正文当成没有正文，避免给上游塞一个空 text 分片。
			return nil, nil
		}
		return []any{textPart(v)}, nil
	case []any:
		parts := make([]any, 0, len(v))
		for _, item := range v {
			p, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content 的元素必须是对象")
			}
			typ, _ := p["type"].(string)
			switch typ {
			case "text", "input_text":
				text, _ := p["text"].(string)
				parts = append(parts, textPart(text))
			case "image_url", "input_image":
				url := imageURLOf(p)
				if url == "" {
					return nil, fmt.Errorf("图片部分缺少 url")
				}
				// Trae 只认 url，detail 之类的字段丢掉。
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			default:
				return nil, fmt.Errorf("不支持的 content 类型: %q", typ)
			}
		}
		return parts, nil
	}
	return nil, fmt.Errorf("content 必须是字符串或对象数组")
}

// imageURLOf 从图片部分里取 url，兼容 image_url 是字符串、是对象、或直接给 url 三种写法。
func imageURLOf(p map[string]any) string {
	switch v := p["image_url"].(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		if s, ok := v["url"].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	if s, ok := p["url"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// textOfParts 收集 parts 里的纯文本，用来拼工具结果的正文。
func textOfParts(parts []any) string {
	var b strings.Builder
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok || pm["type"] != "text" {
			continue
		}
		if s, ok := pm["text"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func textPart(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

// partsOf 取消息的 parts 数组（可能是 nil）。
func partsOf(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// outputLimit 取输出上限：优先 max_completion_tokens，其次 max_tokens。
func outputLimit(src map[string]any) (int, error) {
	raw, ok := src["max_completion_tokens"]
	if !ok || raw == nil {
		raw = src["max_tokens"]
	}
	if raw == nil {
		return 0, nil
	}
	n, ok := number(raw)
	if !ok || n != math.Trunc(n) || n < 1 || n > math.MaxInt32 {
		return 0, fmt.Errorf("max_tokens 必须是正整数")
	}
	return int(n), nil
}

// number 把 JSON 数字读成 float64（也容忍数字字符串）。
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// reasoningEffort 取思考档位：OpenAI 的 low/xhigh 在 Trae 里叫 light/extra_high。
func reasoningEffort(src map[string]any) string {
	raw, ok := src["reasoning_effort"]
	if !ok || raw == nil {
		raw = src["reasoningEffort"]
	}
	s, _ := raw.(string)
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "":
		return ""
	case "low":
		return "light"
	case "xhigh":
		return "extra_high"
	}
	return s
}

// argumentsText 统一 arguments 的表示：字符串用原文，对象序列化，缺省给 "{}"。
func argumentsText(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// isJSONObject 判断一段文本是不是 JSON 对象。
func isJSONObject(s string) bool {
	var obj map[string]any
	return json.Unmarshal([]byte(s), &obj) == nil
}

// strconvQuote 只用于拼极简兜底文本，避免为此引入 strconv。
func strconvQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"tool"`
	}
	return string(b)
}
