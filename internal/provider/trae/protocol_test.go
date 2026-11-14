package trae

import (
	"encoding/json"
	"strings"
	"testing"
)

// sse 把若干 (事件名, 载荷) 拼成一条完整的上游 SSE 流（每帧以空行结束）。
func sse(pairs ...[2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		if p[0] != "" {
			b.WriteString("event: " + p[0] + "\n")
		}
		b.WriteString("data: " + p[1] + "\n\n")
	}
	return b.String()
}

// 走一遍标准 SSE：辅助事件丢弃、正文增量、终止 + 用量。
func TestConvertStreamText(t *testing.T) {
	in := sse(
		[2]string{"progress_notice", `"正在思考"`}, // 裸字符串辅助事件
		[2]string{"heartbeat", `{"ts":1}`},
		[2]string{"output", `{"response":"你好"}`},
		[2]string{"", `{"response":"，世界","event":"output"}`}, // 事件名藏在载荷里
		[2]string{"token_usage", `{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`},
		[2]string{"done", `{"finish_reason":"stop"}`},
	)

	raw, err := ConvertStream(strings.NewReader(in), "gpt-4o")
	if err != nil {
		t.Fatalf("ConvertStream: %v", err)
	}
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
	if got.Object != "chat.completion" {
		t.Fatalf("object=%q", got.Object)
	}
	if content := got.Choices[0].Message.Content; content != "你好，世界" {
		t.Fatalf("content=%q", content)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish=%q", got.Choices[0].FinishReason)
	}
	if got.Usage["total_tokens"] != float64(15) {
		t.Fatalf("usage=%v", got.Usage)
	}
}

// <tool_call> JSON 块：正文先出，工具调用在终止时整体输出。
func TestToolCallJSONBlock(t *testing.T) {
	in := sse(
		[2]string{"output", `{"response":"我查一下。"}`},
		[2]string{"output", mustJSON(map[string]any{
			"response": `<tool_call>{"name":"get_weather","arguments":{"city":"潮州"}}</tool_call>`,
		})},
		[2]string{"done", `{}`},
	)
	tools := []ToolSpec{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}}
	raw, err := ConvertStreamWithTools(strings.NewReader(in), "m", tools)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	choice := got["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "我查一下。" {
		t.Fatalf("content=%v", msg["content"])
	}
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("calls=%v", msg["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("name=%v", fn["name"])
	}
	if !strings.Contains(fn["arguments"].(string), "潮州") {
		t.Fatalf("args=%v", fn["arguments"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish=%v", choice["finish_reason"])
	}
}

// Qwen 风格：<function=name><parameter=k>v</parameter></function>
func TestToolCallQwen(t *testing.T) {
	in := sse(
		[2]string{"output", mustJSON(map[string]any{
			"response": `<function=read_file><parameter=path>/etc/hosts</parameter><parameter=limit>10</parameter></function>`,
		})},
		[2]string{"done", `{}`},
	)
	tools := []ToolSpec{{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}}}`)}}
	raw, err := ConvertStreamWithTools(strings.NewReader(in), "m", tools)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != nil {
		t.Fatalf("content=%v", msg["content"])
	}
	calls := msg["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("args not json: %v", fn["arguments"])
	}
	if args["path"] != "/etc/hosts" {
		t.Fatalf("path=%v (args=%v)", args["path"], args)
	}
	// limit 声明是 integer，所以按 JSON 解析
	if args["limit"] != float64(10) {
		t.Fatalf("limit=%v", args["limit"])
	}
}

// Anthropic 风格：<function_calls><invoke name=""><parameter name=""></...>
func TestToolCallAnthropic(t *testing.T) {
	body := `<function_calls><invoke name="Search"><parameter name="query">炉石</parameter></invoke></function_calls>`
	in := sse(
		[2]string{"output", mustJSON(map[string]any{"response": body})},
		[2]string{"done", `{}`},
	)
	// 声明名是 search，模型写的是 Search —— 归一化后应当能对上
	tools := []ToolSpec{{Name: "search", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}}
	raw, err := ConvertStreamWithTools(strings.NewReader(in), "m", tools)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("calls=%v content=%v", msg["tool_calls"], msg["content"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "search" {
		t.Fatalf("name=%v", fn["name"])
	}
	if !strings.Contains(fn["arguments"].(string), "炉石") {
		t.Fatalf("args=%v", fn["arguments"])
	}
}

// 原生 tool_calls 分片：arguments 必须按 index 拼接。
func TestNativeToolCallFragments(t *testing.T) {
	in := sse(
		[2]string{"output", `{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}`},
		[2]string{"output", `{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"潮州\"}"}}]}`},
		[2]string{"done", `{}`},
	)
	raw, err := ConvertStream(strings.NewReader(in), "m")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"city":"潮州"}` {
		t.Fatalf("args=%v", fn["arguments"])
	}
}

// 代码围栏里的 <function=…> 是正文（与上游插件一致）。
func TestToolMarkerInCodeFenceIsProse(t *testing.T) {
	text := "可以这样写：\n```\n<function=read_file><parameter=path>a</parameter></function>\n```\n完"
	in := sse(
		[2]string{"output", mustJSON(map[string]any{"response": text})},
		[2]string{"done", `{}`},
	)
	tools := []ToolSpec{{Name: "read_file"}}
	raw, err := ConvertStreamWithTools(strings.NewReader(in), "m", tools)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != text {
		t.Fatalf("content=%q", msg["content"])
	}
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("不该有 tool_calls")
	}
}

// 思考增量：顶层 reasoning 是累积值时只吐新增部分（判重靠「字符数 ≥ 16」的启发式）。
func TestCumulativeReasoning(t *testing.T) {
	first := "先让我仔细看看这个问题的全部需求再动手"
	second := first + "，然后给出答案"
	in := sse(
		[2]string{"output", mustJSON(map[string]any{"reasoning": first})},
		[2]string{"output", mustJSON(map[string]any{"reasoning": second})},
		[2]string{"output", `{"response":"好"}`},
		[2]string{"done", `{}`},
	)
	raw, err := ConvertStream(strings.NewReader(in), "m")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != second {
		t.Fatalf("reasoning=%q 期望=%q", msg["reasoning_content"], second)
	}
	if msg["content"] != "好" {
		t.Fatalf("content=%q", msg["content"])
	}
}

// 上游显式报错要映射成 401。
func TestUpstreamErrorMapping(t *testing.T) {
	in := sse([2]string{"error", `{"code":1001,"message":"token expired"}`})
	_, err := ConvertStream(strings.NewReader(in), "m")
	up, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("err 类型=%T %v", err, err)
	}
	if up.Status != 401 {
		t.Fatalf("status=%d", up.Status)
	}
}

// 单事件接口：文本 delta 必须是 {"choices":[{"delta":{"content":"…"}}]} 形状。
func TestTransformEventShape(t *testing.T) {
	st := NewStreamState("m")
	// 裸载荷行立即派发
	frames, err := TransformEvent(`{"response":"你好"}`, st)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames=%v", frames)
	}
	if !strings.HasPrefix(frames[0], "data: {") || !strings.HasSuffix(frames[0], "\n\n") {
		t.Fatalf("frame=%q", frames[0])
	}
	var env struct {
		Object  string `json:"object"`
		Choices []struct {
			Delta        map[string]any `json:"delta"`
			FinishReason any            `json:"finish_reason"`
		} `json:"choices"`
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(frames[0], "data: "), "\n\n")
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		t.Fatalf("unmarshal=%v", err)
	}
	if env.Object != "chat.completion.chunk" || env.Choices[0].Delta["content"] != "你好" {
		t.Fatalf("env=%+v", env)
	}
	if env.Choices[0].FinishReason != nil {
		t.Fatalf("finish 应为 null")
	}

	// 标准 SSE 逐行：data 行压住，空行才派发
	st2 := NewStreamState("m")
	if first, _ := TransformEvent(`data: {"response":"a"}`, st2); len(first) != 0 {
		t.Fatalf("不该立即派发: %v", first)
	}
	f, err := TransformEvent("", st2)
	if err != nil || len(f) != 1 {
		t.Fatalf("frames=%v err=%v", f, err)
	}
	if !strings.Contains(f[0], `"content":"a"`) {
		t.Fatalf("frame=%q", f[0])
	}

	// 终止：finish_reason 与 [DONE]
	f, err = TransformEvent("data: [DONE]", st2)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	f, err = TransformEvent("", st2)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	joined := strings.Join(f, "")
	if !strings.Contains(joined, `"finish_reason":"stop"`) || !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("frames=%v", f)
	}
}

// 帧被截断（没有以空行收尾）要报错。
func TestTruncatedFrame(t *testing.T) {
	if _, err := ConvertStream(strings.NewReader("event: output\ndata: {\"response\":\"x\"}\n"), "m"); err == nil {
		t.Fatalf("应当报错")
	}
}

// 超过 MAX_CALLS 要拒绝。
func TestTooManyCalls(t *testing.T) {
	var b strings.Builder
	for i := 0; i < MaxCalls+1; i++ {
		b.WriteString("event: output\n")
		b.WriteString("data: " + mustJSON(map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    i,
				"id":       "call_" + string(rune('a'+i%26)) + mustJSON(i),
				"function": map[string]any{"name": "f", "arguments": "{}"},
			}},
		}) + "\n\n")
	}
	b.WriteString("event: done\ndata: {}\n\n")
	if _, err := ConvertStream(strings.NewReader(b.String()), "m"); err == nil {
		t.Fatalf("应当报错")
	}
}
