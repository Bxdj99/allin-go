package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"allin/internal/config"
	"allin/internal/provider/antigravity"
	"allin/internal/provider/catpaw"
)

// webChatRT 是网关层测试用的传输层桩：按 URL 路径回预设内容，并记录调用。
//
// 三家的上游域都是常量，没法用 httptest.Server 顶替，所以从 HTTPClient 的
// Transport 这一层拦。
type webChatRT struct {
	mu     sync.Mutex
	paths  []string
	bodies map[string]string
	turn   string // catpaw turn / antigravity 的 SSE 正文
}

func (s *webChatRT) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	if s.bodies == nil {
		s.bodies = map[string]string{}
	}
	s.bodies[r.URL.Path] = string(raw)
	s.mu.Unlock()

	body := s.turn
	if r.URL.Path == catpaw.PathRound || r.URL.Path == catpaw.PathEvent {
		body = `{"code":0,"message":"ok"}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (s *webChatRT) record(prefix string) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	last := ""
	for _, p := range s.paths {
		if strings.HasPrefix(p, prefix) {
			n++
			last = s.bodies[p]
		}
	}
	return n, last
}

// catPawTurnSSE：累积帧（第二帧补「世界」，第三帧补思维链）。
const catPawTurnSSE = "data: {\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"你好\"}]}}\n\n" +
	"data: {\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"你好世界\"}]}}\n\n" +
	"data: {\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"你好世界\"}," +
	"{\"type\":\"text\",\"reasoningContent\":\"想一下\"}]}}\n\n" +
	"data: [DONE]\n\n"

// antigravitySSE：两帧增量 + thought 段 + STOP。
const antigravitySSE = "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hel\"}]}}]}}\n\n" +
	"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}," +
	"{\"text\":\"想\",\"thought\":true}]},\"finishReason\":\"STOP\"}]}\n\n"

// newChatGateway 用一段 providers JSON 建一个带固定口令的网关。
func newChatGateway(t *testing.T, providersJSON string) *Server {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := `{"server":{"host":"127.0.0.1","port":3425,"authKey":"k"},"providers":[` + providersJSON + `]}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	return New(store)
}

// doChat 发一条最小 chat 请求。
func doChat(t *testing.T, srv *Server, model string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []any{map[string]any{"role": "user", "content": "打个招呼"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// 妙手走完整链路：三步会话机 → 累积帧裁成增量 → OpenAI SSE。
func TestCatPawEndToEnd(t *testing.T) {
	rt := &webChatRT{turn: catPawTurnSSE}
	old := catpaw.HTTPClient
	catpaw.HTTPClient = &http.Client{Transport: rt}
	defer func() { catpaw.HTTPClient = old }()

	srv := newChatGateway(t, `{"id":"catpaw","kind":"catpaw",
	  "accounts":[{"id":"a1","access":"tok"}],
	  "models":["deepseek-v4-pro","glm-5.3"]}`)

	rec := doChat(t, srv, "catpaw/deepseek-v4-pro", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`"object":"chat.completion.chunk"`, `"content":"你好"`, `"content":"世界"`, `"reasoning_content":"想一下"`} {
		if !strings.Contains(body, want) {
			t.Errorf("缺 %s：%s", want, body)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("结尾不是 [DONE]：%q", body)
	}
	// round / event / turn 三步都要真的打到上游。
	turns, _ := rt.record(catpaw.PathTurn)
	rounds, roundBody := rt.record(catpaw.PathRound)
	events, _ := rt.record(catpaw.PathEvent)
	if turns != 1 || rounds != 1 || events != 1 {
		t.Errorf("上游调用次数不对: round=%d event=%d turn=%d", rounds, events, turns)
	}
	if !strings.Contains(roundBody, `"source":"CatX"`) {
		t.Errorf("round 体没带 source=CatX：%s", roundBody)
	}
}

// 妙手非流式：整条流聚合回一个 chat.completion。
func TestCatPawNonStream(t *testing.T) {
	rt := &webChatRT{turn: catPawTurnSSE}
	old := catpaw.HTTPClient
	catpaw.HTTPClient = &http.Client{Transport: rt}
	defer func() { catpaw.HTTPClient = old }()

	srv := newChatGateway(t, `{"id":"catpaw","kind":"catpaw",
	  "accounts":[{"id":"a1","access":"tok"}],"models":["glm-5.3"]}`)

	rec := doChat(t, srv, "catpaw/glm-5.3", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	var v struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if v.Object != "chat.completion" || v.Choices[0].Message.Content != "你好世界" {
		t.Errorf("响应不对: %s", rec.Body.String())
	}
}

// 妙手带工具的请求：应当 400（协议承载不了），而不是 502 或者悄悄丢掉工具定义。
func TestCatPawRejectsTools(t *testing.T) {
	rt := &webChatRT{turn: catPawTurnSSE}
	old := catpaw.HTTPClient
	catpaw.HTTPClient = &http.Client{Transport: rt}
	defer func() { catpaw.HTTPClient = old }()

	srv := newChatGateway(t, `{"id":"catpaw","kind":"catpaw",
	  "accounts":[{"id":"a1","access":"tok"}],"models":["glm-5.3"]}`)

	payload := `{"model":"catpaw/glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "chat.completion.chunk") {
		t.Errorf("不该开始写流：%s", rec.Body.String())
	}
	if n, _ := rt.record(catpaw.PathRound); n != 0 {
		t.Errorf("被拒的请求不该打到上游，round 调了 %d 次", n)
	}
}

// Antigravity：配置里的 domain 要被当作 Cloud 项目带进信封。
func TestAntigravityEndToEnd(t *testing.T) {
	rt := &webChatRT{turn: antigravitySSE}
	old := antigravity.HTTPClient
	antigravity.HTTPClient = &http.Client{Transport: rt}
	defer func() { antigravity.HTTPClient = old }()

	srv := newChatGateway(t, `{"id":"antigravity","kind":"antigravity",
	  "accounts":[{"id":"a1","access":"tok","refresh":"rt","domain":"proj-9","expires":4102444800000}],
	  "models":["gemini-3-flash"]}`)

	rec := doChat(t, srv, "antigravity/gemini-3-flash", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"content":"Hel"`, `"content":"lo"`, `"reasoning_content":"想"`} {
		if !strings.Contains(body, want) {
			t.Errorf("缺 %s：%s", want, body)
		}
	}

	// r.URL.Path 不含 query（alt=sse 在 RawQuery 里），所以只按路径前缀取。
	_, reqBody := rt.record("/v1internal:streamGenerateContent")
	var env struct {
		Project string `json:"project"`
		Request struct {
			SessionID string `json:"sessionId"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(reqBody), &env); err != nil {
		t.Fatalf("信封不是 JSON: %v (%s)", err, reqBody)
	}
	if env.Project != "proj-9" {
		t.Errorf("project = %q，配置里的 domain 没被带进信封：%s", env.Project, reqBody)
	}
	if env.Request.SessionID == "" {
		t.Error("sessionId 丢了")
	}
}

// 文心：只接到认证层，必须明确回 501 并说明原因（kind 别名 wenxin_web 也要认）。
func TestWenxinReturnsNotImplemented(t *testing.T) {
	srv := newChatGateway(t, `{"id":"wenxin_web","kind":"wenxin_web",
	  "accounts":[{"id":"a1","access":"BDUSS=abc; STOKEN=def"}],"models":["ernie-4.0-turbo"]}`)

	rec := doChat(t, srv, "wenxin_web/ernie-4.0-turbo", true)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "没有实现") {
		t.Errorf("501 没带原因：%s", rec.Body.String())
	}
}
