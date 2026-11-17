package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// 回归：agent 类请求（上千条消息 + 上百个工具）以前估算偏低，
// 明明会超上游上限却不触发裁剪，导致上游 400「模型请求参数无效」。
//
// 这组数字取自真机上被上游拒掉的一次请求：1167 条消息、183 个工具、
// 约 126 万字符。旧算法按 char/3 算 ≈42 万 token，低于旧阈值 98 万，
// 于是原样转发，被上游拒。
func TestEstimateCatchesLargeAgentRequest(t *testing.T) {
	// 构造等价规模的请求：消息约 112 万字符，工具约 14 万字符
	msgs := make([]map[string]any, 0, 1167)
	big := strings.Repeat("x", 960) // 每条约 1000 字符
	for i := 0; i < 1167; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": big})
	}
	tools := make([]map[string]any, 0, 183)
	toolsBig := strings.Repeat("y", 770) // 每个工具约 770 字符
	for i := 0; i < 183; i++ {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": "f", "description": toolsBig}})
	}
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": msgs, "tools": tools})
	t.Logf("构造请求 %d 字节", len(body))

	res, err := TrimContext(body)
	if err != nil {
		t.Fatalf("裁剪报错: %v", err)
	}
	t.Logf("估算 %d tokens，丢弃 %d 条，trimmed=%v", res.Estimated, res.Dropped, res.Trimmed)
	if !res.Trimmed {
		t.Fatal("这种规模的请求必须触发裁剪，否则上游会 400")
	}
	if res.Estimated > maxPromptTokens {
		t.Fatalf("裁剪后仍超阈值: %d > %d", res.Estimated, maxPromptTokens)
	}
}

// 小请求不该被裁，避免影响正常对话。
func TestEstimateKeepsSmallRequest(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []map[string]any{
			{"role": "user", "content": "你好"},
			{"role": "assistant", "content": "你好，有什么可以帮你"},
			{"role": "user", "content": "介绍一下 HTTP"},
		},
	})
	res, err := TrimContext(body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Trimmed {
		t.Fatal("小请求不应被裁剪")
	}
}
