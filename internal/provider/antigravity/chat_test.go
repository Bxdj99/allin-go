package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// antStub 拦下所有请求，记下 URL / 头 / 请求体，并按预设回 SSE。
type antStub struct {
	mu       sync.Mutex
	url      string
	auth     string
	accept   string
	body     string
	sse      string
	status   int
	requests int
}

func (s *antStub) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests++
	s.url = r.URL.String()
	s.auth = r.Header.Get("Authorization")
	s.accept = r.Header.Get("Accept")
	s.body = string(raw)
	status := s.status
	sse := s.sse
	s.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}, nil
}

func withAntStub(t *testing.T, s *antStub) {
	t.Helper()
	old := HTTPClient
	HTTPClient = &http.Client{Transport: s}
	t.Cleanup(func() { HTTPClient = old })
}

// antSSE 两帧：第一帧套了 {"response": …} 外层（cloudcode-pa 有的版本这么包），
// 第二帧是裸 Gemini 形状，且带一个 thought 段。
const antSSE = "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hel\"}]}}]}}\n\n" +
	"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}," +
	"{\"text\":\"想一下\",\"thought\":true}]},\"finishReason\":\"STOP\"}]}\n\n"

// chatBody 造一个标准 OpenAI chat 请求（带 system，验证 systemInstruction 与 CloakSystem 路径）。
func antChatBody() []byte {
	return []byte(`{"model":"gemini-3-flash","messages":[` +
		`{"role":"system","content":"你是助手"},{"role":"user","content":"打个招呼"}],` +
		`"temperature":0.2,"max_tokens":256}`)
}

// 账号要处在「不需要刷新」的状态：有 Bearer、过期时间在很远的将来、项目已就绪。
func stubAccount() *Account {
	return &Account{
		Access:  "tok",
		Token:   "tok",
		Expires: time.Now().Add(time.Hour).UnixMilli(),
		Project: "proj-1",
	}
}

// 流式：信封要按上游要求拼（project / requestType / requestId / sessionId），
// 响应要变成增量 OpenAI chunk。
func TestRunStream(t *testing.T) {
	st := &antStub{sse: antSSE}
	withAntStub(t, st)

	var out bytes.Buffer
	if err := Run(context.Background(), stubAccount(), "gemini-3-flash", antChatBody(), &out, true); err != nil {
		t.Fatalf("Run: %v", err)
	}

	st.mu.Lock()
	url, auth, accept, body := st.url, st.auth, st.accept, st.body
	st.mu.Unlock()

	if !strings.Contains(url, StreamPath) || !strings.Contains(url, "alt=sse") {
		t.Errorf("请求 URL 不对: %s", url)
	}
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if !strings.Contains(accept, "text/event-stream") {
		t.Errorf("Accept = %q", accept)
	}

	var env struct {
		Model       string `json:"model"`
		Project     string `json:"project"`
		UserAgent   string `json:"userAgent"`
		RequestType string `json:"requestType"`
		RequestID   string `json:"requestId"`
		Request     struct {
			Contents []map[string]any `json:"contents"`
			System   *struct {
				Parts []map[string]any `json:"parts"`
			} `json:"systemInstruction"`
			SessionID string `json:"sessionId"`
			Gen       struct {
				Temperature     *float64 `json:"temperature"`
				MaxOutputTokens int      `json:"maxOutputTokens"`
			} `json:"generationConfig"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("信封不是合法 JSON: %v (%s)", err, body)
	}
	if env.Model != "gemini-3-flash" || env.Project != "proj-1" {
		t.Errorf("model/project 不对: %+v", env)
	}
	if env.UserAgent != EnvelopeUserAgent || env.RequestType != RequestType {
		t.Errorf("userAgent/requestType 不对: %q %q", env.UserAgent, env.RequestType)
	}
	if !strings.HasPrefix(env.RequestID, "agent-") {
		t.Errorf("requestId 不对: %q", env.RequestID)
	}
	if env.Request.SessionID == "" {
		t.Error("sessionId 不能为空")
	}
	if env.Request.System == nil || len(env.Request.System.Parts) == 0 {
		t.Error("systemInstruction 丢了")
	}
	if env.Request.Gen.Temperature == nil || env.Request.Gen.MaxOutputTokens != 256 {
		t.Errorf("generationConfig 不对: %+v", env.Request.Gen)
	}
	if len(env.Request.Contents) == 0 || env.Request.Contents[0]["role"] != "user" {
		t.Errorf("contents 不对: %+v", env.Request.Contents)
	}

	chunks := out.String()
	if !strings.Contains(chunks, `"object":"chat.completion.chunk"`) {
		t.Fatalf("没有 chunk 帧：%s", chunks)
	}
	// 第二帧是纯增量，必须原样追加而不是当成累积全文覆盖。
	if !strings.Contains(chunks, `"content":"Hel"`) || !strings.Contains(chunks, `"content":"lo"`) {
		t.Errorf("增量正文不对：%s", chunks)
	}
	if !strings.Contains(chunks, `"reasoning_content":"想一下"`) {
		t.Errorf("thought 段没转成 reasoning_content：%s", chunks)
	}
	if !strings.Contains(chunks, `"finish_reason":"stop"`) {
		t.Errorf("finishReason 没映射：%s", chunks)
	}
	if !strings.HasSuffix(strings.TrimSpace(chunks), "data: [DONE]") {
		t.Errorf("结尾不是 [DONE]：%q", chunks)
	}
}

// 非流式：聚合成 chat.completion。
func TestRunNonStream(t *testing.T) {
	st := &antStub{sse: antSSE}
	withAntStub(t, st)
	var out bytes.Buffer
	if err := Run(context.Background(), stubAccount(), "gemini-3-flash", antChatBody(), &out, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var v struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("输出不是 JSON: %v (%s)", err, out.String())
	}
	if v.Object != "chat.completion" || len(v.Choices) != 1 {
		t.Fatalf("响应形状不对: %s", out.String())
	}
	if v.Choices[0].Message.Content != "Hello" {
		t.Errorf("正文 = %q", v.Choices[0].Message.Content)
	}
	if v.Choices[0].Message.ReasoningContent != "想一下" {
		t.Errorf("思维链 = %q", v.Choices[0].Message.ReasoningContent)
	}
}

// 工具与图片：本包信封没移植，必须明确拒绝而不是悄悄丢。
func TestRunRejectsToolsAndImages(t *testing.T) {
	st := &antStub{sse: antSSE}
	withAntStub(t, st)

	tools := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if err := Run(context.Background(), stubAccount(), "m", tools, io.Discard, true); err == nil {
		t.Error("带 tools 应当报错")
	}

	img := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x"}}]}]}`)
	if err := Run(context.Background(), stubAccount(), "m", img, io.Discard, true); err == nil {
		t.Error("带图片应当报错")
	}
	if st.requests != 0 {
		t.Errorf("被拒的请求不该发出，实际 %d 次", st.requests)
	}
}

// 上游 429：要带状态码回来（网关据此换号）。
func TestRunUpstreamStatus(t *testing.T) {
	st := &antStub{status: http.StatusTooManyRequests, sse: `{"error":{"code":429,"message":"quota"}}`}
	withAntStub(t, st)
	var out bytes.Buffer
	err := Run(context.Background(), stubAccount(), "m", antChatBody(), &out, true)
	if err == nil {
		t.Fatal("429 应当报错")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型不对: %v", err)
	}
	if ue.StatusCode() != http.StatusTooManyRequests {
		t.Errorf("状态码 = %d", ue.StatusCode())
	}
	if out.Len() != 0 {
		t.Errorf("失败时不该有输出: %s", out.String())
	}
}

// 流里出现 error 帧时，要在流已经开写的前提下尽力告知客户端。
func TestRunStreamErrorFrame(t *testing.T) {
	st := &antStub{sse: "data: {\"error\":{\"code\":500,\"message\":\"boom\",\"status\":\"INTERNAL\"}}\n\n"}
	withAntStub(t, st)
	var out bytes.Buffer
	err := Run(context.Background(), stubAccount(), "m", antChatBody(), &out, true)
	if err == nil {
		t.Fatal("error 帧应当报错")
	}
	if !strings.Contains(out.String(), "boom") {
		t.Errorf("流里没把错误告诉客户端: %q", out.String())
	}
}
