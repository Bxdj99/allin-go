package gateway

import (
	"encoding/json"
	"testing"

	"allin/internal/provider/workbuddy"
)

// 回归：换模型时曾用「原始 body」重写，导致裁剪结果被丢弃，
// 且 Prepare 在已裁产物上再跑一遍，工具名二次映射后
// tool_calls 与 tool 结果对不上，上游报 11148。
func TestRewriteModelKeepsTrim(t *testing.T) {
	// 构造一个含大量历史 + 工具调用链的请求
	var msgs []map[string]any
	msgs = append(msgs, map[string]any{"role": "system", "content": "sys"})
	msgs = append(msgs, map[string]any{"role": "user", "content": "开始"})
	big := make([]byte, 2000)
	for i := range big {
		big[i] = 'x'
	}
	for i := 0; i < 800; i++ {
		id := "call_" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{
				{"id": id, "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": string(big)},
		)
	}
	orig, _ := json.Marshal(map[string]any{
		"model": "workbuddy/deepseek-v4.1-flash", "messages": msgs, "stream": true,
	})
	t.Logf("原始请求 %d 字节", len(orig))

	prepared, err := workbuddy.Prepare(orig, "")
	if err != nil {
		t.Fatal(err)
	}
	trimmed, err := workbuddy.TrimContext(prepared)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("裁剪后 %d 字节，丢 %d 条", len(trimmed.Body), trimmed.Dropped)

	// 换模型：必须在裁剪产物上改
	rewritten, err := rewriteModel(trimmed.Body, "hy3")
	if err != nil {
		t.Fatal(err)
	}
	if len(rewritten) > len(trimmed.Body)*2 {
		t.Fatalf("换模型后体积暴涨（%d -> %d），说明裁剪被丢弃了",
			len(trimmed.Body), len(rewritten))
	}

	// 配对必须仍然完好
	var m map[string]json.RawMessage
	json.Unmarshal(rewritten, &m)
	var outMsgs []json.RawMessage
	json.Unmarshal(m["messages"], &outMsgs)

	declared := map[string]bool{}
	for _, raw := range outMsgs {
		var x struct {
			ToolCalls []struct{ ID string `json:"id"` } `json:"tool_calls"`
		}
		json.Unmarshal(raw, &x)
		for _, c := range x.ToolCalls {
			declared[c.ID] = true
		}
	}
	for _, raw := range outMsgs {
		var x struct {
			Role   string `json:"role"`
			ToolID string `json:"tool_call_id"`
		}
		json.Unmarshal(raw, &x)
		if x.Role == "tool" && !declared[x.ToolID] {
			t.Fatalf("换模型后出现孤立 tool 结果: %s", x.ToolID)
		}
	}
	t.Logf("换模型后 %d 字节，配对完好", len(rewritten))
}
