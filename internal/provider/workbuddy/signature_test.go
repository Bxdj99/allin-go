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
		{200, `thinking_signature_invalid`, false},
		{500, `thinking_signature_invalid`, false},
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

const sigRefusal = `{"internal_code":"11159"}`

// ── 能删的情况：空且无签名的 thinking 块 ──────────────────────────

func TestEmptyUnsignedThinkingRemoved(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":""},{"type":"text","text":"可见回答"}`)

	d := DecideSignatureRetry(400, sigRefusal, body, false)
	if !d.Retry {
		t.Fatalf("空且无签名的 thinking 应当可以删除，原因=%s", d.Reason)
	}
	s := string(d.Body)
	if strings.Contains(s, `"thinking":""`) {
		t.Error("空 thinking 块应被删除")
	}
	if !strings.Contains(s, "可见回答") {
		t.Error("可见文本应保留")
	}
}

// 删空后补空 text 块，避免上游拒空 content
func TestEmptyUnsignedThinkingEmptyFill(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":""}`)

	d := DecideSignatureRetry(400, sigRefusal, body, false)
	if !d.Retry {
		t.Fatalf("应可重试，原因=%s", d.Reason)
	}
	if !strings.Contains(string(d.Body), `"type":"text"`) {
		t.Error("删空后应补一个空 text 块")
	}
}

// ── 不能删的情况：会变成有损重试 ────────────────────────────────

// 带签名的 thinking 是上游签发过的，删了会让签名与内容对不上。
func TestSignedThinkingBlocksRetry(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":"","signature":"abc"},{"type":"text","text":"答"}`)

	d := DecideSignatureRetry(400, sigRefusal, body, false)
	if d.Retry {
		t.Fatal("带签名的 thinking 不应被删除（有损重试）")
	}
	if d.Reason != StopProtectedHistory {
		t.Errorf("原因 = %s, want %s", d.Reason, StopProtectedHistory)
	}
}

// 有实际正文的 thinking 删了就丢内容。
func TestNonEmptyThinkingBlocksRetry(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":"真实的推理过程"},{"type":"text","text":"答"}`)

	d := DecideSignatureRetry(400, sigRefusal, body, false)
	if d.Retry {
		t.Fatal("有内容的 thinking 不应被删除")
	}
	if d.Reason != StopRetryWouldBeLossy {
		t.Errorf("原因 = %s, want %s", d.Reason, StopRetryWouldBeLossy)
	}
}

// 带额外字段（如 cache_control）时删掉会改变语义。
func TestThinkingWithExtraFieldsBlocksRetry(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":"","cache_control":{"type":"ephemeral"}}`)

	d := DecideSignatureRetry(400, sigRefusal, body, false)
	if d.Retry {
		t.Fatal("带额外字段的 thinking 不应被删除")
	}
	if d.Reason != StopRetryWouldBeLossy {
		t.Errorf("原因 = %s", d.Reason)
	}
}

// redacted_thinking 是受保护内容。
func TestRedactedThinkingBlocksRetry(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":""},{"type":"redacted_thinking","data":"abc"}`)

	d := DecideSignatureRetry(400, sigRefusal, body, false)
	if d.Retry {
		t.Fatal("含 redacted_thinking 时不应重试")
	}
	if d.Reason != StopProtectedHistory {
		t.Errorf("原因 = %s, want %s", d.Reason, StopProtectedHistory)
	}
}

// ── 状态码与前置条件 ───────────────────────────────────────────

// 只有 400 才重试：其它 4xx 改请求体也不会过。
func TestOnlyStatus400Retries(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":""}`)

	for _, status := range []int{401, 403, 422, 429, 500} {
		d := DecideSignatureRetry(status, sigRefusal, body, false)
		if d.Retry {
			t.Errorf("status %d 不该重试", status)
		}
		if d.Reason != StopStatusNotRetryable {
			t.Errorf("status %d 原因 = %s", status, d.Reason)
		}
	}
	if d := DecideSignatureRetry(400, sigRefusal, body, false); !d.Retry {
		t.Errorf("400 应可重试，原因=%s", d.Reason)
	}
}

// 企业账号跳过签名重试（与上游一致）。
func TestEnterpriseAccountSkips(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":""}`)

	d := DecideSignatureRetry(400, sigRefusal, body, true)
	if d.Retry {
		t.Fatal("企业账号不应做签名重试")
	}
	if d.Reason != StopEnterpriseSkipped {
		t.Errorf("原因 = %s", d.Reason)
	}
}

// 不是签名问题就不重试。
func TestNonSignatureErrorNoRetry(t *testing.T) {
	body := mkBody(`{"type":"thinking","thinking":""}`)
	d := DecideSignatureRetry(400, `{"code":11128,"msg":"unapproved channel"}`, body, false)
	if d.Retry {
		t.Fatal("非签名错误不应重试")
	}
	if d.Reason != StopNotSignatureRejection {
		t.Errorf("原因 = %s", d.Reason)
	}
}

// 没有 thinking 时必须返回 false，否则会重复发同样的请求
func TestNothingToRetryWhenNoThinking(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`)

	d := DecideSignatureRetry(400, sigRefusal, body, false)
	if d.Retry {
		t.Fatal("没有可删的块时不应重发")
	}
	if d.Reason != StopNothingToRetry {
		t.Errorf("原因 = %s, want %s", d.Reason, StopNothingToRetry)
	}
	if WithoutThinkingBlocks(body) != nil {
		t.Error("旧接口也应返回 nil")
	}
}

// ── 其它内容一律原样保留 ───────────────────────────────────────

func TestToolUseKept(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[
		{"type":"thinking","thinking":""},
		{"type":"tool_use","id":"t1","name":"read_file","input":{}}]}]}`)

	d := DecideSignatureRetry(400, sigRefusal, body, false)
	if !d.Retry {
		t.Fatalf("应可重试，原因=%s", d.Reason)
	}
	var m map[string]any
	if err := json.Unmarshal(d.Body, &m); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if !strings.Contains(string(d.Body), "read_file") {
		t.Error("tool_use 应保留")
	}
}

// 非 assistant 消息里的 thinking 不动。
func TestUserMessageUntouched(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"thinking","thinking":""}]}]}`)

	d := DecideSignatureRetry(400, sigRefusal, body, false)
	if d.Retry {
		t.Fatal("user 消息里的内容不该被改动，因此没有可删的块")
	}
}
