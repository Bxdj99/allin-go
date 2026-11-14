package oai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 普通字符串 content：文本要取到，工具/非文本标记都不能误判。
func TestParseChatPlain(t *testing.T) {
	body := []byte(`{"model":"m1","messages":[{"role":"system","content":"sys"},{"role":"user","content":"你好"}],` +
		`"temperature":0.3,"max_tokens":128}`)
	req, err := ParseChat(body)
	if err != nil {
		t.Fatalf("ParseChat: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[1].Text != "你好" || req.Messages[1].Role != "user" {
		t.Fatalf("消息解析不对: %+v", req.Messages)
	}
	if req.HasTools || req.NonText {
		t.Fatalf("不该标成有工具/非文本: %+v", req)
	}
	if req.MaxTokens != 128 || req.Temperature == nil || *req.Temperature != 0.3 {
		t.Fatalf("参数解析不对: %+v", req)
	}
}

// 分段 content + tools + max_completion_tokens：文本段拼起来，图片段标 nonText。
func TestParseChatPartsAndTools(t *testing.T) {
	body := []byte(`{"model":"m1","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"http://x/1.png"}}]}],` +
		`"tools":[{"type":"function","function":{"name":"f"}}],"max_completion_tokens":64}`)
	req, err := ParseChat(body)
	if err != nil {
		t.Fatalf("ParseChat: %v", err)
	}
	if !req.HasTools {
		t.Error("tools 没被识别")
	}
	if !req.NonText {
		t.Error("图片段没被标成 nonText")
	}
	if req.Messages[0].Text != "看图" {
		t.Errorf("文本段 = %q", req.Messages[0].Text)
	}
	if req.MaxTokens != 64 {
		t.Errorf("max_completion_tokens 没兜住: %d", req.MaxTokens)
	}
}

// 空 messages 必须报错，不能给上游发一个没有内容的请求。
func TestParseChatRejectsEmptyMessages(t *testing.T) {
	if _, err := ParseChat([]byte(`{"model":"m1","messages":[]}`)); err == nil {
		t.Fatal("空 messages 应当报错")
	}
	if _, err := ParseChat([]byte(`不是 JSON`)); err == nil {
		t.Fatal("非 JSON 应当报错")
	}
}

// Accumulator 要同时吃「累积快照」和「纯增量」两种形态。
func TestAccumulator(t *testing.T) {
	cum := &Accumulator{}
	if got := cum.Feed("你好"); got != "你好" {
		t.Errorf("累积首帧 = %q", got)
	}
	if got := cum.Feed("你好世界"); got != "世界" {
		t.Errorf("累积增量 = %q", got)
	}
	if cum.Full() != "你好世界" {
		t.Errorf("累积全文 = %q", cum.Full())
	}

	inc := &Accumulator{}
	if got := inc.Feed("Hel"); got != "Hel" {
		t.Errorf("增量首帧 = %q", got)
	}
	if got := inc.Feed("lo"); got != "lo" {
		t.Errorf("增量第二帧 = %q", got)
	}
	if inc.Full() != "Hello" {
		t.Errorf("增量全文 = %q", inc.Full())
	}
	if got := inc.Feed(""); got != "" {
		t.Errorf("空串应当返回空串，得到 %q", got)
	}
}

// 流式帧：每行都是 data: 前缀，首帧带 role，末帧带 finish_reason，最后是 [DONE]。
func TestStreamFrames(t *testing.T) {
	var buf bytes.Buffer
	st := NewStream(&buf, "m1")
	if err := st.Delta("你好", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Delta("", "思考中"); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(""); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Errorf("不是 chunk 帧: %s", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("首帧缺 role: %s", out)
	}
	if !strings.Contains(out, `"reasoning_content":"思考中"`) {
		t.Errorf("思维链丢了: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("缺 finish_reason: %s", out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "data: ") || !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Errorf("SSE 形状不对: %q", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Errorf("出现非 data: 行: %q", line)
		}
	}
}

// 非流式响应：形状要齐全（对象类型、正文、finish_reason、usage）。
func TestCompletionShape(t *testing.T) {
	raw := Completion("m1", "正文", "思考")
	var v struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("Completion 不是合法 JSON: %v", err)
	}
	if v.Object != "chat.completion" || v.Model != "m1" {
		t.Errorf("对象类型/模型不对: %+v", v)
	}
	if len(v.Choices) != 1 || v.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices 不对: %+v", v.Choices)
	}
	if v.Choices[0].Message["content"] != "正文" || v.Choices[0].Message["reasoning_content"] != "思考" {
		t.Errorf("message 不对: %+v", v.Choices[0].Message)
	}
	if v.Usage["total_tokens"] != 0 {
		t.Errorf("usage 应当是 0: %+v", v.Usage)
	}
}
