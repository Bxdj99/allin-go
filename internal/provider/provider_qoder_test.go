package provider

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"allin/internal/config"
	"allin/internal/provider/qoder"
)

// TestBuildRequestQoder 校验 Qoder 这条链的接线：URL 带固定查询参数、头走 COSY 签名
// （且删掉了三个拿不到的设备头）、请求体是编码后的信封。
func TestBuildRequestQoder(t *testing.T) {
	body := []byte(`{"model":"qmodel_demo","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	acct := &config.Account{ID: "a1", Name: "测试", UID: "u1", MachineID: "machine1", Email: "a@b.c"}

	req, err := BuildRequest(KindQoder, "https://gateway.qoder.com.cn", "/chat/completions", "jobtoken", acct, body, nil)
	if err != nil {
		t.Fatalf("BuildRequest 失败: %v", err)
	}

	if got, want := req.URL.String(), qoder.ChatURL; got != want {
		t.Fatalf("URL 不对:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(req.URL.RawQuery, "AgentId=agent_common") || !strings.Contains(req.URL.RawQuery, "Encode=1") {
		t.Fatalf("查询参数不对: %s", req.URL.RawQuery)
	}
	if auth := req.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Fatalf("Authorization 不是 COSY 签名: %q", auth)
	}
	if req.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("Accept 应为事件流: %q", req.Header.Get("Accept"))
	}
	for _, h := range []string{"Cosy-ClientIp", "Cosy-MachineToken", "Cosy-MachineType"} {
		if v := req.Header.Get(h); v != "" {
			t.Fatalf("%s 必须删掉，实际 %q", h, v)
		}
	}
	if req.Header.Get("Cosy-MachineId") != "machine1" {
		t.Fatalf("Cosy-MachineId 不对: %q", req.Header.Get("Cosy-MachineId"))
	}
	if req.Header.Get("X-Model-Key") != "qmodel_demo" {
		t.Fatalf("X-Model-Key 应兜底成请求体里的 model: %q", req.Header.Get("X-Model-Key"))
	}

	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("读请求体失败: %v", err)
	}
	text := string(raw)
	if strings.HasPrefix(text, "{") {
		t.Fatal("请求体应该是编码后的线上串，不是明文 JSON")
	}
	var env struct {
		Business struct {
			Product string `json:"product"`
			Stage   string `json:"stage"`
			ModelKV string `json:"model_key"`
		} `json:"business"`
		Messages []struct {
			Role     string `json:"role"`
			Contents []struct {
				Text string `json:"text"`
			} `json:"contents"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(qoder.DecodeBody(text)), &env); err != nil {
		t.Fatalf("解回的信封不是合法 JSON: %v", err)
	}
	if env.Business.Product != "app" || env.Business.Stage != "start" || env.Business.ModelKV != "qmodel_demo" {
		t.Fatalf("business 不对: %+v", env.Business)
	}
	found := false
	for _, m := range env.Messages {
		if m.Role == "user" && len(m.Contents) > 0 && m.Contents[0].Text == "你好" {
			found = true
		}
	}
	if !found {
		t.Fatal("user 回合缺 contents 或内容丢失")
	}
}

// TestBuildRequestQoderNeedsMachineID 校验缺 machineId 时给出明确错误，而不是发一个
// 上游必拒的请求。
func TestBuildRequestQoderNeedsMachineID(t *testing.T) {
	body := []byte(`{"model":"qmodel_demo","messages":[{"role":"user","content":"hi"}]}`)
	acct := &config.Account{ID: "a1", UID: "u1"}
	if _, err := BuildRequest(KindQoder, "", "/chat/completions", "jobtoken", acct, body, nil); err == nil {
		t.Fatal("缺 machineId 时应该报错")
	}
}
