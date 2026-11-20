package workbuddy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 造一个含 N 组工具调用的请求
func buildToolConv(n int, toolChars int) []byte {
	var msgs []map[string]any
	msgs = append(msgs, map[string]any{"role": "user", "content": "开始任务"})
	big := strings.Repeat("x", toolChars)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{
				{"id": id, "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": big},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "最后问题"})
	b, _ := json.Marshal(map[string]any{"model": "m", "messages": msgs})
	return b
}

func checkPairsOK(t *testing.T, body []byte) {
	t.Helper()
	var m map[string]json.RawMessage
	json.Unmarshal(body, &m)
	var msgs []json.RawMessage
	json.Unmarshal(m["messages"], &msgs)

	declared := map[string]bool{}
	for _, raw := range msgs {
		var x struct {
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		}
		json.Unmarshal(raw, &x)
		for _, c := range x.ToolCalls {
			declared[c.ID] = true
		}
	}
	for _, raw := range msgs {
		var x struct {
			Role   string `json:"role"`
			ToolID string `json:"tool_call_id"`
		}
		json.Unmarshal(raw, &x)
		if x.Role == "tool" && !declared[x.ToolID] {
			t.Fatalf("压缩后出现孤立 tool 结果: %s", x.ToolID)
		}
	}
}

func TestCompressReducesSize(t *testing.T) {
	body := buildToolConv(20, 3000)
	out, n := CompressHistory(body)
	t.Logf("原始 %d 字节 → 压缩后 %d 字节，压缩 %d 组", len(body), len(out), n)
	if n == 0 {
		t.Fatal("应压缩掉一些工具组")
	}
	if len(out) >= len(body) {
		t.Fatal("压缩后体积应减小")
	}
	checkPairsOK(t, out)
}

// 关键：压缩后配对必须完好，否则上游 11148
func TestCompressKeepsPairing(t *testing.T) {
	for _, groups := range []int{10, 20, 50} {
		body := buildToolConv(groups, 2500)
		out, n := CompressHistory(body)
		checkPairsOK(t, out)
		t.Logf("%d 组 → 压缩 %d 组，配对完好", groups, n)
	}
}

// 组数很少时不该动
func TestCompressSkipsShortConversation(t *testing.T) {
	body := buildToolConv(3, 3000)
	out, n := CompressHistory(body)
	if n != 0 {
		t.Fatalf("组数少时不该压缩，实际压了 %d 组", n)
	}
	if string(out) != string(body) {
		t.Fatal("未压缩时应原样返回")
	}
}

// 最近几组必须保留原文（当前任务上下文）
func TestCompressKeepsRecentGroups(t *testing.T) {
	body := buildToolConv(20, 3000)
	out, _ := CompressHistory(body)

	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	var msgs []json.RawMessage
	json.Unmarshal(m["messages"], &msgs)

	// 统计还有多少 tool 消息（最近 6 组应完整保留）
	toolMsgs := 0
	for _, raw := range msgs {
		var x struct {
			Role string `json:"role"`
		}
		json.Unmarshal(raw, &x)
		if x.Role == "tool" {
			toolMsgs++
		}
	}
	if toolMsgs < 6 {
		t.Fatalf("最近 6 组应保留原文，实际只剩 %d 条 tool 消息", toolMsgs)
	}
	t.Logf("保留了 %d 条 tool 原文", toolMsgs)
}

// 压缩结果应能通过 Prepare 与裁剪而不报错
func TestCompressThenPipeline(t *testing.T) {
	body := buildToolConv(20, 3000)
	compressed, _ := CompressHistory(body)
	prepared, err := Prepare(compressed, "")
	if err != nil {
		t.Fatalf("压缩后 Prepare 失败: %v", err)
	}
	trimmed, err := TrimContext(prepared)
	if err != nil {
		t.Fatalf("压缩后裁剪失败: %v", err)
	}
	checkPairsOK(t, trimmed.Body)
	t.Logf("压缩 %d → Prepare %d → 裁剪 %d 字节", len(compressed), len(prepared), len(trimmed.Body))
}
