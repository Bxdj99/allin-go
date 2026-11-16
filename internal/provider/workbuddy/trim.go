package workbuddy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 上下文窗口裁剪。
//
// 上游限制：prompt 总量上限 1,048,576 tokens。
// 超了直接返回：
//
//	{"code":11115,"msg":"prompt is too long: 1202122 tokens > 1048576 maximum"}
//
// Agent 会话（尤其 Eta 这种）很容易积累到几千条消息、几十个工具，几轮之后就会撞墙。
// 网关应该主动裁剪，而不是把错误抛给客户端——用户看到的只是「参数无效」。
//
// 裁剪策略：
//  1. 永远保留 system 消息（人格/指令，删了行为就变了）
//  2. 永远保留最近 N 条消息（当前任务上下文）
//  3. 从最老的非 system 消息开始丢，直到估算 token 进预算
//  4. tool 消息与它对应的 assistant tool_calls 必须成对处理，
//     孤立的 tool 消息上游会拒（400 tool_call_id 找不到）

const (
	// maxPromptTokens 是上游硬上限（1048576），留 5% 余量给工具 schema 与我们的注入。
	maxPromptTokens = 980_000
	// minKeepMessages 是无论怎么裁都要保留的最近消息条数。
	minKeepMessages = 20
	// charsPerToken 是粗略换算；中文约 1.5 字/token，英文约 4 字符/token，
	// 混排取 3 比较稳（宁可多裁一点也不要被上游拒）。
	charsPerToken = 3
)

// TrimResult 是裁剪结果。
type TrimResult struct {
	Body     []byte
	Dropped  int  // 丢掉的消息条数
	Estimated int // 裁剪后的估算 token
	Trimmed  bool
}

// TrimContext 检查并裁剪请求体里的 messages，使其不超过上游上下文上限。
func TrimContext(body []byte) (*TrimResult, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	raw, ok := m["messages"]
	if !ok {
		return &TrimResult{Body: body}, nil
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return &TrimResult{Body: body}, nil
	}

	// 估算整体 token（含 tools，因为 tools 也占上下文）
	totalChars := 0
	for _, msg := range msgs {
		totalChars += len(msg)
	}
	if t, ok := m["tools"]; ok {
		totalChars += len(t)
	}
	est := totalChars / charsPerToken
	if est <= maxPromptTokens {
		return &TrimResult{Body: body, Estimated: est}, nil
	}

	// 需要裁剪：先分离 system 与其余
	var systems []json.RawMessage
	var rest []json.RawMessage
	for _, msg := range msgs {
		var head struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(msg, &head)
		if head.Role == "system" || head.Role == "developer" {
			systems = append(systems, msg)
		} else {
			rest = append(rest, msg)
		}
	}

	sysChars := 0
	for _, s := range systems {
		sysChars += len(s)
	}
	toolsChars := 0
	if t, ok := m["tools"]; ok {
		toolsChars = len(t)
	}
	budgetChars := (maxPromptTokens - 50_000) * charsPerToken // 再留 5万 token 给输出
	availChars := budgetChars - sysChars - toolsChars

	// 从尾部往前累加，能放下多少就保留多少
	var kept []json.RawMessage
	acc := 0
	for i := len(rest) - 1; i >= 0; i-- {
		n := len(rest[i])
		if acc+n > availChars && len(kept) >= minKeepMessages {
			break
		}
		kept = append([]json.RawMessage{rest[i]}, kept...)
		acc += n
	}

	// 修掉被裁断的 tool 关系：
	// 如果保留的第一条是 tool 消息，说明它的 assistant tool_calls 被裁掉了，
	// 这种孤立 tool 消息上游会拒，直接丢。
	for len(kept) > 0 {
		var head struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(kept[0], &head)
		if head.Role != "tool" {
			break
		}
		kept = kept[1:]
	}

	out := append(append([]json.RawMessage{}, systems...), kept...)
	buf, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	m["messages"] = buf
	nb, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}

	est2 := (sysChars + acc + toolsChars) / charsPerToken
	return &TrimResult{
		Body:      nb,
		Dropped:   len(rest) - len(kept),
		Estimated: est2,
		Trimmed:   true,
	}, nil
}

// ContextLimitError 是上游因超长拒绝时的可读提示。
func ContextLimitError(upstreamMsg string) string {
	// 从 "prompt is too long: 1202122 tokens > 1048576 maximum" 里抠数字
	var have, max int
	if _, err := fmt.Sscanf(upstreamMsg, "prompt is too long: %d tokens > %d maximum", &have, &max); err == nil {
		return fmt.Sprintf("会话上下文超长（%d tokens，上限 %d）。网关已尝试裁剪，但仍超限；"+
			"请在客户端新建会话，或减少工具数量", have, max)
	}
	return strings.TrimSpace(upstreamMsg)
}
