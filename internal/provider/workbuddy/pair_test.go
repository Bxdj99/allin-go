package workbuddy

import (
	"encoding/json"
	"testing"
)

func msg(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Fatalf("测试用例 JSON 非法: %s", s)
	}
	return json.RawMessage(s)
}

// 完整配对必须原样保留
func TestSanitizeKeepsCompletePairs(t *testing.T) {
	in := []json.RawMessage{
		msg(t, `{"role":"user","content":"hi"}`),
		msg(t, `{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`),
		msg(t, `{"role":"tool","tool_call_id":"c1","content":"ok"}`),
		msg(t, `{"role":"assistant","content":"done"}`),
	}
	out := sanitizeToolPairs(in)
	if len(out) != 4 {
		t.Fatalf("完整配对不该被删，实际剩 %d 条", len(out))
	}
}

// 场景 1：assistant 声明了调用，但结果被裁掉 → 整组丢弃
func TestSanitizeDropsOrphanToolCalls(t *testing.T) {
	in := []json.RawMessage{
		msg(t, `{"role":"user","content":"hi"}`),
		msg(t, `{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`),
		// c1 的结果缺失
		msg(t, `{"role":"assistant","content":"done"}`),
	}
	out := sanitizeToolPairs(in)
	for _, raw := range out {
		var m struct {
			ToolCalls json.RawMessage `json:"tool_calls"`
		}
		json.Unmarshal(raw, &m)
		if len(m.ToolCalls) > 0 {
			t.Fatal("悬空的 tool_calls 应被删除，否则上游 400 code 11148")
		}
	}
	if len(out) != 2 {
		t.Fatalf("应只剩 user + assistant 文本，实际 %d 条", len(out))
	}
}

// 场景 2：tool 结果留下但声明被裁掉 → 结果丢弃
func TestSanitizeDropsOrphanToolResult(t *testing.T) {
	in := []json.RawMessage{
		msg(t, `{"role":"user","content":"hi"}`),
		msg(t, `{"role":"tool","tool_call_id":"c9","content":"orphan"}`),
		msg(t, `{"role":"assistant","content":"done"}`),
	}
	out := sanitizeToolPairs(in)
	for _, raw := range out {
		var m struct {
			Role string `json:"role"`
		}
		json.Unmarshal(raw, &m)
		if m.Role == "tool" {
			t.Fatal("孤立 tool 结果应被丢弃")
		}
	}
}

// 场景 3：多个调用只有部分结果 → 整组丢掉
func TestSanitizeDropsPartiallyAnsweredCalls(t *testing.T) {
	in := []json.RawMessage{
		msg(t, `{"role":"user","content":"hi"}`),
		msg(t, `{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]}`),
		msg(t, `{"role":"tool","tool_call_id":"c1","content":"ok"}`),
		// c2 缺失
	}
	out := sanitizeToolPairs(in)
	for _, raw := range out {
		var m struct {
			Role      string          `json:"role"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		}
		json.Unmarshal(raw, &m)
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Fatalf("部分应答的调用组应整体丢弃，残留: %s", string(raw)[:60])
		}
	}
}

// 场景 4：普通对话（无工具）不受影响
func TestSanitizeLeavesPlainChatAlone(t *testing.T) {
	in := []json.RawMessage{
		msg(t, `{"role":"user","content":"a"}`),
		msg(t, `{"role":"assistant","content":"b"}`),
		msg(t, `{"role":"user","content":"c"}`),
	}
	if out := sanitizeToolPairs(in); len(out) != 3 {
		t.Fatalf("普通对话不该被改动，实际 %d 条", len(out))
	}
}
