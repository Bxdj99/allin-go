package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"allin/internal/config"
)

// 端到端：起一个假的 Trae 上游，走完整的 /v1/chat/completions 链路。
// 验证「端点换对了、请求体换成 Trae 的形状、响应流被逐帧转换」。
func TestTraeEndToEnd(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		if ct := r.Header.Get("Accept"); !strings.Contains(ct, "text/event-stream") {
			t.Errorf("上游 Accept = %q，应要求事件流", ct)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, traeSSE)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := `{"server":{"host":"127.0.0.1","port":3425,"authKey":"k"},
	  "providers":[{
	    "id":"trae","kind":"trae","baseUrl":"` + upstream.URL + `",
	    "accounts":[{"id":"a1","access":"tok","uid":"u1","deviceId":"d1","machineId":"m1"}],
	    "models":["Doubao-Seed-1.6"]
	  }]}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv := New(store)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// --- 流式 ---
	rec := post(`{"model":"trae/Doubao-Seed-1.6","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("流式状态码 = %d，正文 = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/agent/v3/llm_utils_chat" {
		t.Errorf("上游路径 = %q", gotPath)
	}
	if gotBody["config_name"] != "Doubao-Seed-1.6" || gotBody["stream"] != true {
		t.Errorf("上游请求体 = %v", gotBody)
	}
	if msgs, _ := gotBody["messages"].([]any); len(msgs) != 1 {
		t.Errorf("上游 messages = %v", gotBody["messages"])
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"object":"chat.completion.chunk"`) || !strings.Contains(out, "你好") {
		t.Errorf("流式输出没转成 OpenAI chunk：%s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: [DONE]") {
		t.Errorf("流式输出没有 [DONE] 收尾")
	}

	// --- 非流式（本地聚合）---
	rec = post(`{"model":"trae/Doubao-Seed-1.6","stream":false,"messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("非流式状态码 = %d，正文 = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("非流式结果不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q", got.Object)
	}
	if len(got.Choices) == 0 || got.Choices[0].Message.Content != "你好，世界" {
		t.Errorf("choices = %+v", got.Choices)
	}
}
