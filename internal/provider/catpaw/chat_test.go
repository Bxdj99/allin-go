package catpaw

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
)

// catPawStub 用自定义 RoundTripper 顶掉真实网络：妙手的域是常量，只能在传输层拦。
type catPawStub struct {
	mu       sync.Mutex
	paths    []string
	bodies   map[string]string
	turnSSE  string
	turnCode int // 非 0 表示让 turn 回这个 HTTP 状态（模拟上游报错）
}

func (s *catPawStub) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	if s.bodies == nil {
		s.bodies = map[string]string{}
	}
	s.bodies[r.URL.Path] = string(raw)
	s.mu.Unlock()

	switch r.URL.Path {
	case PathRound, PathEvent:
		return stubResp(`{"code":0,"message":"ok"}`, http.StatusOK), nil
	case PathTurn:
		if s.turnCode != 0 {
			return stubResp(`{"code":500,"message":"boom"}`, s.turnCode), nil
		}
		return stubResp(s.turnSSE, http.StatusOK), nil
	default:
		return stubResp(`{"code":404,"message":"unknown path"}`, http.StatusNotFound), nil
	}
}

func (s *catPawStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths)
}

func stubResp(body string, status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// withStub 把包里的 HTTPClient 换成桩，测试结束再换回去。
func withStub(t *testing.T, s *catPawStub) {
	t.Helper()
	old := HTTPClient
	HTTPClient = &http.Client{Transport: s}
	t.Cleanup(func() { HTTPClient = old })
}

// turnSSE 是典型的妙手累积帧：每帧给到目前的全文，中间夹一帧只有思维链的。
const turnSSE = "data: {\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"你好\"}]}}\n\n" +
	"data: {\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"你好\"}," +
	"{\"type\":\"text\",\"reasoningContent\":\"想一下\"}]}}\n\n" +
	"data: {\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"你好世界\"}," +
	"{\"type\":\"text\",\"reasoningContent\":\"想一下\"}]}}\n\n" +
	"data: [DONE]\n\n"

// chatBody 造一个标准 OpenAI chat 请求。
func chatBody(extra string) []byte {
	return []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"打个招呼"}]` + extra + `}`)
}

// 流式：三步会话机走完，累积帧要变成增量 chunk。
func TestRunStream(t *testing.T) {
	st := &catPawStub{turnSSE: turnSSE}
	withStub(t, st)

	var out bytes.Buffer
	err := Run(context.Background(), &Account{Token: "tok"}, "deepseek-v4-pro", chatBody(""), &out, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 三步都要发到，且顺序是 round → event → turn。
	st.mu.Lock()
	paths := append([]string(nil), st.paths...)
	roundBody := st.bodies[PathRound]
	turnBody := st.bodies[PathTurn]
	st.mu.Unlock()
	if len(paths) != 3 || paths[0] != PathRound || paths[1] != PathEvent || paths[2] != PathTurn {
		t.Fatalf("请求序列不对: %v", paths)
	}
	for _, want := range []string{`"source":"CatX"`, `"mode":"CATX_APP"`} {
		if !strings.Contains(roundBody, want) {
			t.Errorf("round 请求体缺 %s：%s", want, roundBody)
		}
		if !strings.Contains(turnBody, want) {
			t.Errorf("turn 请求体缺 %s：%s", want, turnBody)
		}
	}

	body := out.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Fatalf("没有 chunk 帧：%s", body)
	}
	// 累积帧要裁成增量：第二帧只有思维链，第三帧只补「世界」。
	if !strings.Contains(body, `"content":"你好"`) {
		t.Errorf("首帧正文丢了：%s", body)
	}
	if !strings.Contains(body, `"content":"世界"`) {
		t.Errorf("增量没裁出来（可能把累积全文又发了一遍）：%s", body)
	}
	if !strings.Contains(body, `"reasoning_content":"想一下"`) {
		t.Errorf("思维链丢了：%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("结尾不是 [DONE]：%q", body)
	}
}

// 同一份 conversationId 必须出现在 round 和 turn 里（上游靠它认会话）。
func TestRunConversationIDSame(t *testing.T) {
	st := &catPawStub{turnSSE: turnSSE}
	withStub(t, st)
	var out bytes.Buffer
	if err := Run(context.Background(), &Account{Token: "tok"}, "glm-5.3", chatBody(""), &out, true); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	var round, turn struct {
		ConversationID string `json:"conversationId"`
	}
	if err := json.Unmarshal([]byte(st.bodies[PathRound]), &round); err != nil {
		t.Fatalf("round 体解析失败: %v", err)
	}
	if err := json.Unmarshal([]byte(st.bodies[PathTurn]), &turn); err != nil {
		t.Fatalf("turn 体解析失败: %v", err)
	}
	if round.ConversationID == "" || round.ConversationID != turn.ConversationID {
		t.Errorf("conversationId 不一致: round=%q turn=%q", round.ConversationID, turn.ConversationID)
	}
}

// 非流式：聚合成一个 chat.completion。
func TestRunNonStream(t *testing.T) {
	st := &catPawStub{turnSSE: turnSSE}
	withStub(t, st)
	var out bytes.Buffer
	if err := Run(context.Background(), &Account{Token: "tok"}, "deepseek-v4-pro", chatBody(""), &out, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var v struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("输出不是 JSON: %v (%s)", err, out.String())
	}
	if v.Object != "chat.completion" || len(v.Choices) != 1 {
		t.Fatalf("响应形状不对: %s", out.String())
	}
	if v.Choices[0].Message.Content != "你好世界" {
		t.Errorf("正文 = %q", v.Choices[0].Message.Content)
	}
	if v.Choices[0].Message.ReasoningContent != "想一下" {
		t.Errorf("思维链 = %q", v.Choices[0].Message.ReasoningContent)
	}
	if v.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", v.Choices[0].FinishReason)
	}
}

// 带工具的请求必须在发网络请求之前就被拒（不能悄悄丢掉工具定义）。
func TestRunRejectsTools(t *testing.T) {
	st := &catPawStub{turnSSE: turnSSE}
	withStub(t, st)
	body := chatBody(`,"tools":[{"type":"function","function":{"name":"get_time"}}]`)
	var out bytes.Buffer
	err := Run(context.Background(), &Account{Token: "tok"}, "auto", body, &out, true)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("应当返回 ErrUnsupported，得到 %v", err)
	}
	if n := st.count(); n != 0 {
		t.Errorf("被拒的请求不该发出，实际发了 %d 次", n)
	}
}

// 上游明确报错时要带状态码回来，网关据此换号。
func TestRunUpstreamStatus(t *testing.T) {
	st := &catPawStub{turnSSE: turnSSE, turnCode: http.StatusTooManyRequests}
	withStub(t, st)
	var out bytes.Buffer
	err := Run(context.Background(), &Account{Token: "tok"}, "auto", chatBody(""), &out, true)
	if err == nil {
		t.Fatal("上游 429 应当报错")
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

// 没有 token 的账号直接报需要重新登录，不浪费一次网络请求。
func TestRunNoToken(t *testing.T) {
	st := &catPawStub{turnSSE: turnSSE}
	withStub(t, st)
	var out bytes.Buffer
	if err := Run(context.Background(), &Account{}, "auto", chatBody(""), &out, true); !errors.Is(err, ErrSignInRequired) {
		t.Fatalf("应当返回 ErrSignInRequired，得到 %v", err)
	}
	if n := st.count(); n != 0 {
		t.Errorf("无凭据不该发请求，实际 %d 次", n)
	}
}
