package provider

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"allin/internal/config"
)

// BuildRequest 在 Trae 分支里必须：换端点、换请求体、保留伪装头。
func TestBuildRequestTrae(t *testing.T) {
	body := []byte(`{"model":"Doubao-Seed-1.6","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	acct := &config.Account{ID: "a1", Access: "tok", UID: "u1", DeviceID: "dev1", MachineID: "mach1"}

	req, err := BuildRequest(KindTrae, "https://trae-api-cn.mchost.guru", "/chat/completions", acct.Key(), acct, body, nil)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if want := "https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat"; req.URL.String() != want {
		t.Errorf("URL = %q，想要 %q", req.URL.String(), want)
	}
	if auth := req.Header.Get("Authorization"); !strings.HasPrefix(auth, "Cloud-IDE-JWT ") {
		t.Errorf("Authorization = %q", auth)
	}

	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("读请求体: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, raw)
	}
	if m["config_name"] != "Doubao-Seed-1.6" {
		t.Errorf("config_name = %v", m["config_name"])
	}
	if m["function"] != "chat_v3" {
		t.Errorf("function = %v", m["function"])
	}
	if m["stream"] != true {
		t.Errorf("stream 必须强制为 true，实际 %v", m["stream"])
	}
	if s, _ := m["session_id"].(string); len(s) != 36 {
		t.Errorf("session_id = %q", s)
	}
	if _, ok := m["top_p"]; ok {
		t.Errorf("不该出现的字段被带进去了")
	}
}

// 供应商固定头里的 X-Allin-* 只当内部参数用，不能真的发给上游。
func TestBuildRequestTraeInternalHeaders(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	acct := &config.Account{ID: "a1", Access: "tok", UID: "u1"}
	extra := map[string]string{
		"X-Allin-Function":       "solo_agent",
		"X-Allin-Context-Window": "128000",
	}
	req, err := BuildRequest(KindTrae, "https://trae-api-cn.mchost.guru", "/chat/completions", acct.Key(), acct, body, extra)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := req.Header.Get("X-Allin-Function"); got != "" {
		t.Errorf("内部头被发给上游了: %q", got)
	}
	raw, _ := io.ReadAll(req.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	if m["function"] != "solo_agent" {
		t.Errorf("function = %v", m["function"])
	}
	ctx, _ := m["user_message_context"].(map[string]any)
	info, _ := ctx["model_info"].(map[string]any)
	if info["prompt_max_tokens"] != float64(128000) {
		t.Errorf("prompt_max_tokens = %v", info["prompt_max_tokens"])
	}
}
