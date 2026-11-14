package gateway

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"allin/internal/config"
	"allin/internal/provider/deepseekweb"
)

// dsStub 模拟 DeepSeek 网页版的四个端点：建会话、领 PoW 挑战、completion。
type dsStub struct {
	t         *testing.T
	salt      string
	expire    int64
	answer    int64
	diffs     int64
	challenge string // 由 salt/expire/answer 算出的正确摘要
	reply     string // completion 返回的 SSE 正文

	mu       sync.Mutex
	sessions int
	requests []map[string]any
	headers  []http.Header
}

// newDSStub 造一个桩服务。PoW 的正确答案固定为 answer，challenge 用同一个
// Keccak 变体现算，这样网关里的 SolvePoW 能真的解出来。
func newDSStub(t *testing.T, frames ...string) (*dsStub, *httptest.Server) {
	st := &dsStub{t: t, salt: "stub-salt", expire: 1700000000, answer: 7, diffs: 64}
	st.reply = strings.Join(frames, "")

	answer := []byte(fmt.Sprintf("%s_%d_%d", st.salt, st.expire, st.answer))
	sum := deepseekweb.Hash(answer, 23)
	st.challenge = hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc(deepseekweb.PathSessionCreate, func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.sessions++
		id := fmt.Sprintf("sess-%d", st.sessions)
		st.mu.Unlock()
		writeStubJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"biz_code": 0,
				"biz_data": map[string]any{"chat_session": map[string]any{"id": id}},
			},
		})
	})
	mux.HandleFunc(deepseekweb.PathPowChallenge, func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"biz_code": 0,
				"biz_data": map[string]any{
					"challenge": map[string]any{
						"algorithm":  "DeepSeekHashV1",
						"challenge":  st.challenge,
						"salt":       st.salt,
						"signature":  "stub-signature",
						"difficulty": st.diffs,
						"expire_at":  st.expire,
					},
				},
			},
		})
	})
	mux.HandleFunc(deepseekweb.PathCompletion, func(w http.ResponseWriter, r *http.Request) {
		// 校验 PoW 头：解出来的 answer 必须等于桩里预设的那个
		raw, err := base64.StdEncoding.DecodeString(r.Header.Get(deepseekweb.HeaderPow))
		if err != nil {
			st.t.Errorf("completion 缺少合法 PoW 头: %v", err)
		}
		var pow struct {
			Answer     int64  `json:"answer"`
			TargetPath string `json:"target_path"`
		}
		if err := json.Unmarshal(raw, &pow); err != nil || pow.Answer != st.answer {
			st.t.Errorf("PoW 答案不对: %+v err=%v", pow, err)
		}
		if pow.TargetPath != deepseekweb.PathCompletion {
			st.t.Errorf("PoW target_path 不对: %s", pow.TargetPath)
		}

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.requests = append(st.requests, body)
		st.headers = append(st.headers, r.Header.Clone())
		st.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(st.reply))
	})

	return st, httptest.NewServer(mux)
}

// challenge 字段：桩自己算出来的正确摘要。
func (st *dsStub) snapshot() (sessions int, reqs []map[string]any, hdrs []http.Header) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sessions, append([]map[string]any(nil), st.requests...), append([]http.Header(nil), st.headers...)
}

func writeStubJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// dsFrames 一轮典型的 patch 流：先思考片段，再正文两段，最后状态收尾。
func dsFrames() []string {
	return []string{
		`data: {"p":"response/fragments","o":"SET","v":[{"type":"THINK","content":""},{"type":"RESPONSE","content":""}]}` + "\n\n",
		`data: {"p":"response/fragments/0/content","o":"APPEND","v":"先想一想"}` + "\n\n",
		`data: {"p":"response/fragments/1/content","o":"APPEND","v":"你好"}` + "\n\n",
		`data: {"p":"response/fragments/1/content","o":"APPEND","v":"，世界"}` + "\n\n",
		`data: {"p":"response/message_id","v":101}` + "\n\n",
		`data: {"p":"response/status","v":"FINISHED"}` + "\n\n",
		"event: close\ndata: {}\n\n",
	}
}

// newDSGateway 造一个只配了 DeepSeek 网页版的网关，并返回首次启动生成的口令。
func newDSGateway(t *testing.T, base string) (*Server, string) {
	t.Helper()
	store, err := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("建配置失败: %v", err)
	}
	cfg := store.Snapshot()
	cfg.Providers = []*config.Provider{{
		ID:      "ds",
		Name:    "DeepSeek 网页版",
		Kind:    "deepseek-web",
		BaseURL: base,
		Accounts: []*config.Account{
			{ID: "a1", Access: "stub-token", Enabled: boolPtr(true)},
		},
		Models: []string{"deepseek-chat", "deepseek-reasoner"},
	}}
	if err := store.Save(cfg); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	return New(store), store.Snapshot().Server.AuthKey
}

func boolPtr(b bool) *bool { return &b }

// postChat 发一次 chat/completions，返回状态码与响应体。
func postChat(t *testing.T, s *Server, key, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestDeepSeekWebNonStream 非流式：整条 patch 流要聚合成一个 chat.completion。
func TestDeepSeekWebNonStream(t *testing.T) {
	stub, srv := newDSStub(t, dsFrames()...)
	defer srv.Close()
	s, key := newDSGateway(t, srv.URL)

	code, out := postChat(t, s, key, `{"model":"ds/deepseek-chat","stream":false,
		"messages":[{"role":"user","content":"你好"}]}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，响应体 %s", code, out)
	}

	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v / %s", err, out)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q，期望 chat.completion", got.Object)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices 数量 = %d", len(got.Choices))
	}
	if msg := got.Choices[0].Message; msg.Content != "你好，世界" || msg.ReasoningContent != "先想一想" {
		t.Errorf("正文/思考链不对: content=%q reasoning=%q", msg.Content, msg.ReasoningContent)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.Choices[0].FinishReason)
	}

	// 上游必须是「建会话一次 + 一次 completion」，且 prompt 被摊平
	sessions, reqs, hdrs := stub.snapshot()
	if sessions != 1 || len(reqs) != 1 {
		t.Fatalf("建会话 %d 次、completion %d 次，期望各 1", sessions, len(reqs))
	}
	if reqs[0]["prompt"] != "你好" {
		t.Errorf("prompt 摊平结果 = %q", reqs[0]["prompt"])
	}
	if reqs[0]["chat_session_id"] != "sess-1" {
		t.Errorf("chat_session_id = %v", reqs[0]["chat_session_id"])
	}
	// 网页前端指纹头必须齐（缺一个上游就按第三方调用拒绝）
	h := hdrs[0]
	for _, k := range []string{"X-Client-Platform", "X-Client-Version", "X-Client-Bundle-Id", "X-Client-Locale"} {
		if h.Get(k) == "" {
			t.Errorf("缺少指纹头 %s", k)
		}
	}
	if h.Get("Referer") != deepseekweb.Origin+"/a/chat/" {
		t.Errorf("referer = %q", h.Get("Referer"))
	}
}

// TestDeepSeekWebStream 流式：patch 增量的 APPEND 语义要变成标准 chunk。
func TestDeepSeekWebStream(t *testing.T) {
	_, srv := newDSStub(t, dsFrames()...)
	defer srv.Close()
	s, key := newDSGateway(t, srv.URL)

	code, out := postChat(t, s, key, `{"model":"ds/deepseek-chat","stream":true,
		"messages":[{"role":"user","content":"你好"}]}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，响应体 %s", code, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Errorf("流没有以 [DONE] 收尾: %q", out)
	}

	var text, reasoning strings.Builder
	finish := ""
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk 不是 JSON: %v / %s", err, payload)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %q", chunk.Object)
		}
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
			reasoning.WriteString(c.Delta.ReasoningContent)
			if c.FinishReason != nil {
				finish = *c.FinishReason
			}
		}
	}
	if text.String() != "你好，世界" {
		t.Errorf("拼出来的正文 = %q", text.String())
	}
	if reasoning.String() != "先想一想" {
		t.Errorf("拼出来的思考链 = %q", reasoning.String())
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q", finish)
	}
}

// TestDeepSeekWebSessionReuse 第二轮必须复用上一轮的 chat_session_id + parent_message_id，
// 而不是又建一个新会话。
func TestDeepSeekWebSessionReuse(t *testing.T) {
	stub, srv := newDSStub(t, dsFrames()...)
	defer srv.Close()
	s, key := newDSGateway(t, srv.URL)

	// 用 prompt_cache_key 明确指定会话，两轮共用一条上游会话
	first := `{"model":"ds/deepseek-chat","stream":false,"prompt_cache_key":"chat-42",
		"messages":[{"role":"user","content":"第一轮"}]}`
	second := `{"model":"ds/deepseek-chat","stream":false,"prompt_cache_key":"chat-42",
		"messages":[{"role":"user","content":"第一轮"},{"role":"assistant","content":"你好，世界"},{"role":"user","content":"第二轮"}]}`

	if code, out := postChat(t, s, key, first); code != http.StatusOK {
		t.Fatalf("第一轮状态码 %d: %s", code, out)
	}
	if code, out := postChat(t, s, key, second); code != http.StatusOK {
		t.Fatalf("第二轮状态码 %d: %s", code, out)
	}

	sessions, reqs, _ := stub.snapshot()
	if sessions != 1 {
		t.Errorf("建会话 %d 次，期望只建 1 次（第二轮应复用）", sessions)
	}
	if len(reqs) != 2 {
		t.Fatalf("completion 次数 = %d", len(reqs))
	}
	if reqs[0]["parent_message_id"] != nil {
		t.Errorf("首轮 parent_message_id 应为 null，实际 %v", reqs[0]["parent_message_id"])
	}
	if reqs[1]["chat_session_id"] != "sess-1" {
		t.Errorf("第二轮 chat_session_id = %v，期望 sess-1", reqs[1]["chat_session_id"])
	}
	// json.Number 解出来的 101
	if fmt.Sprint(reqs[1]["parent_message_id"]) != "101" {
		t.Errorf("第二轮 parent_message_id = %v，期望 101", reqs[1]["parent_message_id"])
	}
}

// TestDSConvoKey 会话标识的优先级：prompt_cache_key → user → messages 哈希。
func TestDSConvoKey(t *testing.T) {
	a := dsConvoKey([]byte(`{"model":"m","prompt_cache_key":"a","user":"u","messages":[{"role":"user","content":"1"}]}`))
	if a != "n:a" {
		t.Errorf("优先 prompt_cache_key 失败: %s", a)
	}
	b := dsConvoKey([]byte(`{"model":"m","user":"u","messages":[{"role":"user","content":"1"}]}`))
	if b != "n:u" {
		t.Errorf("回退 user 失败: %s", b)
	}
	c1 := dsConvoKey([]byte(`{"model":"m","messages":[{"role":"user","content":"1"}]}`))
	c2 := dsConvoKey([]byte(`{"model":"m","messages":[{"role":"user","content":"1"}]}`))
	c3 := dsConvoKey([]byte(`{"model":"m","messages":[{"role":"user","content":"2"}]}`))
	if c1 != c2 {
		t.Errorf("同一段 messages 应得同一标识: %s / %s", c1, c2)
	}
	if c1 == c3 {
		t.Errorf("不同 messages 不该撞同一标识: %s", c1)
	}
}

// TestDeepSeekWebReasonerMode 模型别名要如实映射成网页版模式：
// deepseek-reasoner 走 expert + 深度思考，deepseek-chat 走 default。
func TestDeepSeekWebReasonerMode(t *testing.T) {
	stub, srv := newDSStub(t, dsFrames()...)
	defer srv.Close()
	s, key := newDSGateway(t, srv.URL)

	if code, out := postChat(t, s, key, `{"model":"ds/deepseek-reasoner","stream":false,
		"messages":[{"role":"user","content":"想一想"}]}`); code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", code, out)
	}

	_, reqs, _ := stub.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("completion 次数 = %d", len(reqs))
	}
	if reqs[0]["model_type"] != "expert" {
		t.Errorf("model_type = %v，期望 expert", reqs[0]["model_type"])
	}
	if reqs[0]["thinking_enabled"] != true {
		t.Errorf("thinking_enabled = %v，期望 true", reqs[0]["thinking_enabled"])
	}
}
