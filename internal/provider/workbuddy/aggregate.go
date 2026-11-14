// Package workbuddy —— SSE 聚合器。
//
// WorkBuddy 的 /v2/chat/completions **不接受非流式请求**
// （实测返回 {"code":11101,"msg":"Non-stream chat request is currently not supported"}）。
//
// 所以网关必须两头适配：
//   - 对上游：一律发 stream:true
//   - 对客户端：如果客户端要的是非流式，就把 SSE 流聚合成一个标准 chat.completion 再返回
//
// 这里实现聚合。工具调用要按 index 累加 arguments 片段，不能简单覆盖。
package workbuddy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// sseChunk 是 OpenAI 风格流式分片里我们关心的部分。
type sseChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
	// WorkBuddy 的业务错误会夹在流里
	Code       int    `json:"code"`
	Msg        string `json:"msg"`
	RequestID  string `json:"requestId"`
}

// Aggregate 把 SSE 流聚合成一条完整的 chat.completion 响应。
//
// model 是客户端看到的模型名（上游返回的可能是别名，这里统一成本地名）。
func Aggregate(r io.Reader, model string) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)

	var (
		id        string
		content   strings.Builder
		reasoning strings.Builder
		toolCalls []map[string]any
		byIndex   = map[int]map[string]any{}
		order     []int
		usage     map[string]any
		finish    string
		bizErr    string
	)

	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}

		var ch sseChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			continue // 非 JSON 分片（心跳、注释）直接跳过
		}
		// 业务错误混在流里
		if ch.Code != 0 && ch.Msg != "" {
			bizErr = fmt.Sprintf("%s (code %d)", ch.Msg, ch.Code)
			continue
		}
		if ch.ID != "" {
			id = ch.ID
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != "" {
				content.WriteString(c.Delta.Content)
			}
			// 某些模型把思考过程放在 reasoning_content
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
			for _, tc := range c.Delta.ToolCalls {
				entry, ok := byIndex[tc.Index]
				if !ok {
					entry = map[string]any{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]any{
							"name":      tc.Function.Name,
							"arguments": tc.Function.Arguments,
						},
					}
					byIndex[tc.Index] = entry
					order = append(order, tc.Index)
					continue
				}
				fn := entry["function"].(map[string]any)
				if tc.Function.Name != "" {
					fn["name"] = tc.Function.Name
				}
				// arguments 是分片累加，必须拼接
				if tc.Function.Arguments != "" {
					fn["arguments"] = fn["arguments"].(string) + tc.Function.Arguments
				}
				if tc.ID != "" {
					entry["id"] = tc.ID
				}
			}
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return nil, err
	}

	if bizErr != "" && content.Len() == 0 && len(byIndex) == 0 {
		return nil, fmt.Errorf("WorkBuddy 拒绝了请求: %s", bizErr)
	}

	for _, idx := range order {
		toolCalls = append(toolCalls, byIndex[idx])
	}

	msg := map[string]any{"role": "assistant", "content": content.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if finish == "" {
			finish = "tool_calls"
		}
	}
	if finish == "" {
		finish = "stop"
	}
	if id == "" {
		id = "chatcmpl-allin"
	}
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}

	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"model":   model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   usage,
	}
	_ = reasoning
	return json.Marshal(out)
}

// ForceStream 把请求体里的 stream 强制改成 true（WorkBuddy 唯一接受的形态）。
func ForceStream(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["stream"] = json.RawMessage(`true`)
	// 顺带要求上游返回用量（有些实现靠这个字段）
	if _, ok := m["stream_options"]; !ok {
		m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}
	return json.Marshal(m)
}
