package qoder

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildChatBody 校验「标准 chat 请求 → 信封 → EncodeBody」这条链：
// 线上串必须能解回合法 JSON，且关键字段（business/parameters/model_config、
// user 回合的 contents、request_set_id 与 session_id 的区分）都对得上。
func TestBuildChatBody(t *testing.T) {
	in := map[string]any{
		"model":      "qmodel_demo",
		"max_tokens": 2048,
		"messages": []any{
			map[string]any{"role": "system", "content": "你是运维助手"},
			map[string]any{"role": "user", "content": "今天天气怎么样？"},
			map[string]any{"role": "assistant", "content": "晴"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "那明天呢"},
			}},
		},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "查天气",
			},
		}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}

	wire, err := BuildChatBody(raw, "qmodel_demo", "")
	if err != nil {
		t.Fatalf("BuildChatBody 失败: %v", err)
	}
	if wire == "" || strings.HasPrefix(wire, "{") {
		t.Fatalf("线上串应该已被编码，实际前 40 字节: %q", wire[:min(40, len(wire))])
	}

	text := DecodeBody(wire)
	var env chatEnvelope
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("解回的信封不是合法 JSON: %v\n%s", err, snippet([]byte(text)))
	}

	if env.Business.Product != "app" || env.Business.Type != "agent" || env.Business.Stage != "start" {
		t.Fatalf("business 基本字段不对: %+v", env.Business)
	}
	if env.Business.Version != CosyVersion {
		t.Fatalf("business.version 应为 %s，实际 %s", CosyVersion, env.Business.Version)
	}
	if env.Business.ModelKey != "qmodel_demo" || env.ModelConfig.Key != "qmodel_demo" {
		t.Fatalf("model key 两处必须一致: business=%q model_config=%q", env.Business.ModelKey, env.ModelConfig.Key)
	}
	if env.Business.BeginAt <= 0 {
		t.Fatalf("business.begin_at 应为毫秒时间戳，实际 %d", env.Business.BeginAt)
	}
	if env.RequestID == "" || env.ChatRecordID != env.RequestID || env.RequestSetID == "" {
		t.Fatalf("三个 id 不对: %s / %s / %s", env.RequestID, env.ChatRecordID, env.RequestSetID)
	}
	if env.RequestSetID == env.SessionID {
		t.Fatal("request_set_id 是任务 id，不应等于 session_id")
	}
	if env.Parameters.MaxTokens != 2048 || !env.Parameters.EnableThinking {
		t.Fatalf("parameters 不对: %+v", env.Parameters)
	}
	if env.ModelConfig.Source != DefaultModelSource {
		t.Fatalf("model_config.source 兜底失败: %q", env.ModelConfig.Source)
	}

	// system：第一条消息承载「固定提示 + 客户端 system」，且 system 段与它一致。
	if len(env.Messages) != 4 {
		t.Fatalf("消息条数应为 4（system + user + assistant + user），实际 %d", len(env.Messages))
	}
	if env.Messages[0].Role != "system" || !strings.Contains(env.Messages[0].Content[0].Text, QoderSystem) {
		t.Fatalf("首条 system 回合不对: %+v", env.Messages[0])
	}
	if !strings.Contains(env.Messages[0].Content[0].Text, "你是运维助手") {
		t.Fatal("客户端 system 没有合并进 system 文本")
	}
	if len(env.System) != 1 || env.System[0].Text != env.Messages[0].Content[0].Text {
		t.Fatal("system 段与首条 system 回合必须一致")
	}

	// user 回合必须同时带 content 和 contents。
	for _, m := range env.Messages {
		if m.Role != "user" {
			continue
		}
		if len(m.Contents) == 0 || m.Contents[0].Text != m.Content[0].Text {
			t.Fatalf("user 回合缺少 contents: %+v", m)
		}
	}
	last := env.Messages[len(env.Messages)-1]
	if last.Role != "user" || last.Content[0].Text != "那明天呢" {
		t.Fatalf("最后一条用户消息不对: %+v", last)
	}

	// 工具：缺 parameters 时要补空 object schema。
	if len(env.Tools) != 1 || string(env.Tools[0].Function.Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("工具声明不对: %+v", env.Tools)
	}

	// 确定性：同样的对话开头 → 同样的 session_id。
	wire2, err := BuildChatBody(raw, "qmodel_demo", "")
	if err != nil {
		t.Fatalf("第二次 BuildChatBody 失败: %v", err)
	}
	var env2 chatEnvelope
	if err := json.Unmarshal([]byte(DecodeBody(wire2)), &env2); err != nil {
		t.Fatalf("第二次解信封失败: %v", err)
	}
	if env2.SessionID != env.SessionID || env2.RequestSetID != env.RequestSetID {
		t.Fatalf("同一段对话的 session/task 应稳定: %q vs %q", env.SessionID, env2.SessionID)
	}
	if env2.RequestID == env.RequestID {
		t.Fatal("request_id 每轮应该是新的")
	}
}

// TestBuildChatBodyRejects 覆盖两个必须拒绝的入参。
func TestBuildChatBodyRejects(t *testing.T) {
	raw := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := BuildChatBody(raw, "auto", ""); err == nil {
		t.Fatal("auto 是占位 key，应该拒绝")
	}
	if _, err := BuildChatBody(raw, "", ""); err == nil {
		t.Fatal("缺模型 key 应该拒绝")
	}
	if _, err := BuildChatBody([]byte(`{"model":"x"}`), "qmodel_demo", ""); err == nil {
		t.Fatal("缺 messages 应该拒绝")
	}
}

// sseOf 把若干帧拼成上游那种 SSE 文本。
func sseOf(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	return b.String()
}

// chunkOf 造一个 OpenAI 形态的增量帧。
func chunkOf(content string, finish string) string {
	delta := map[string]any{}
	if content != "" {
		delta["content"] = content
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	} else {
		choice["finish_reason"] = nil
	}
	chunk := map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1700000000,
		"model": "qmodel_demo", "choices": []any{choice},
	}
	out, _ := json.Marshal(chunk)
	return string(out)
}

// envelopeOf 把 chunk 包进上游那层 {statusCodeValue, body} 信封。
func envelopeOf(chunk string) string {
	out, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": chunk})
	return string(out)
}

// TestRelayStream 校验流式解帧：信封里包的 chunk、裸 chunk、被编码过的帧、
// 以及 [DONE] 都应该能解出来并重写成标准事件。
func TestRelayStream(t *testing.T) {
	// 上游可能给「信封 + 未编码 body」「裸 chunk」「信封 + 编码 body」三种混合形态。
	encodedChunk := EncodeBody(chunkOf("！", ""))
	src := sseOf(
		envelopeOf(chunkOf("你", "")),
		chunkOf("好", ""),
		envelopeOf(encodedChunk),
		envelopeOf(chunkOf("", "stop")),
		"[DONE]",
	)

	var dst strings.Builder
	if err := RelayStream(&dst, strings.NewReader(src)); err != nil {
		t.Fatalf("RelayStream 失败: %v", err)
	}
	out := dst.String()

	events := strings.Split(strings.TrimRight(out, "\n"), "\n\n")
	if len(events) != 5 {
		t.Fatalf("应写出 5 个事件，实际 %d：\n%s", len(events), out)
	}
	for i, ev := range events {
		if !strings.HasPrefix(ev, "data: ") {
			t.Fatalf("第 %d 个事件不是 data: 行: %q", i, ev)
		}
	}
	if events[4] != "data: [DONE]" {
		t.Fatalf("最后一个事件应为 [DONE]，实际 %q", events[4])
	}
	var first struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(events[0], "data: ")), &first); err != nil {
		t.Fatalf("写出的事件不是合法 JSON: %v", err)
	}
	if first.Choices[0].Delta.Content != "你" {
		t.Fatalf("首帧内容不对: %+v", first)
	}
	if strings.Contains(out, "statusCodeValue") {
		t.Fatal("信封字段不该泄漏给客户端")
	}
}

// TestRelayStreamError 校验上游错误帧（状态码非 200）会被报出来。
func TestRelayStreamError(t *testing.T) {
	env, _ := json.Marshal(map[string]any{"statusCodeValue": 500, "body": `{"message":"boom"}`})
	var dst strings.Builder
	err := RelayStream(&dst, strings.NewReader(sseOf(string(env))))
	if err == nil {
		t.Fatal("状态码非 200 时应返回错误")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("错误里应带状态码: %v", err)
	}
}

// TestAggregate 校验非流式聚合：正文、思考、工具调用与收尾原因。
func TestAggregate(t *testing.T) {
	toolChunk, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1700000000,
		"model": "qmodel_demo",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city":`},
			}},
		}, "finish_reason": nil}},
	})
	// 参数是分片到达的，和正文一样要按 index 拼起来。
	argsChunk, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1700000000,
		"model": "qmodel_demo",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    0,
				"function": map[string]any{"arguments": `"杭州"}`},
			}},
		}, "finish_reason": "tool_calls"}},
	})
	src := sseOf(
		envelopeOf(chunkOf("晴", "")),
		envelopeOf(chunkOf("天", "")),
		envelopeOf(string(toolChunk)),
		envelopeOf(string(argsChunk)),
		"[DONE]",
	)

	out, err := Aggregate(strings.NewReader(src), "qmodel_demo")
	if err != nil {
		t.Fatalf("Aggregate 失败: %v", err)
	}
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v\n%s", err, out)
	}
	if got.Object != "chat.completion" || got.Model != "qmodel_demo" || len(got.Choices) != 1 {
		t.Fatalf("聚合结构不对: %+v", got)
	}
	msg := got.Choices[0].Message
	if msg.Content == nil || *msg.Content != "晴天" {
		t.Fatalf("正文聚合不对: %q", *msg.Content)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"city":"杭州"}` {
		t.Fatalf("工具调用拼装不对: %+v", msg.ToolCalls)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("有工具调用时 finish_reason 应为 tool_calls，实际 %q", got.Choices[0].FinishReason)
	}
}

// TestAggregateEmpty 校验空流直接报错，而不是回一个空回复。
func TestAggregateEmpty(t *testing.T) {
	if _, err := Aggregate(strings.NewReader(sseOf("[DONE]")), "qmodel_demo"); err == nil {
		t.Fatal("没有任何内容时应报错")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
