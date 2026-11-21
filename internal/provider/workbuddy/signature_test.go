package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// 真机反馈里的错误文案
func TestIsSignatureRefusal(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":"thinking_signature_invalid"}`, true},
		{400, `{"msg":"invalid signature in thinking block"}`, true},
		{400, `{"internal_code":"11159"}`, true},
		{400, `{"msg":"block is bound to a different conversation"}`, true},
		{400, `{"code":11128,"msg":"unapproved channel"}`, false},
		{200, `thinking_signature_invalid`, false},  // 非 4xx 不算
		{500, `thinking_signature_invalid`, false},  // 5xx 不算
		{400, `{"msg":"quota exhausted"}`, false},
		{400, ``, false},
	}
	for _, c := range cases {
		if got := IsSignatureRefusal(c.status, c.body); got != c.want {
			t.Errorf("IsSignatureRefusal(%d, %.40q) = %v, 期望 %v", c.status, c.body, got, c.want)
		}
	}
}

func mkBody(blocks string) []byte {
	return []byte(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[` + blocks + `]},
		{"role":"user","content":"继续"}
	]}`)
}

// 去掉 thinking，保留文本
func TestWithoutThinkingBlocks(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":"内部推理","signature":"BAD"},
		{"type":"text","text":"可见回答"}`)
	out := WithoutThinkingBlocks(body)
	if out == nil {
		t.Fatal("应返回新 body")
	}
	s := string(out)
	if strings.Contains(s, `"type":"thinking"`) {
		t.Fatal("thinking 块应被删除")
	}
	if !strings.Contains(s, "可见回答") {
		t.Fatal("可见文本应保留")
	}
}

// 没有 thinking 时必须返回 nil，否则会重复发同样的请求
func TestWithoutThinkingBlocksNilWhenNone(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`)
	if out := WithoutThinkingBlocks(body); out != nil {
		t.Fatal("没有 thinking 时应返回 nil")
	}
}

// 删空后补空 text 块，避免上游拒空 content
func TestWithoutThinkingBlocksEmptyFill(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":"x","signature":"BAD"}`)
	out := WithoutThinkingBlocks(body)
	if out == nil {
		t.Fatal("应返回新 body")
	}
	if !strings.Contains(string(out), `"type":"text"`) {
		t.Fatal("删空后应补一个空 text 块")
	}
}

// redacted_thinking 不能删
func TestRedactedThinkingKept(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":"x","signature":"BAD"},
		{"type":"redacted_thinking","data":"abc"}`)
	out := WithoutThinkingBlocks(body)
	if out == nil {
		t.Fatal("应返回新 body")
	}
	if !strings.Contains(string(out), "redacted_thinking") {
		t.Fatal("redacted_thinking 应保留")
	}
}

// 工具调用必须原样保留
func TestToolCallsKept(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[
		{"type":"thinking","thinking":"x","signature":"BAD"},
		{"type":"tool_use","id":"t1","name":"read_file","input":{}}]}]}`)
	out := WithoutThinkingBlocks(body)
	var m map[string]any
	json.Unmarshal(out, &m)
	if !strings.Contains(string(out), "read_file") {
		t.Fatal("tool_use 应保留")
	}
}
