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
	// maxPromptTokens 是裁剪阈值，不是上游硬上限。
	// 上游报的上限是 1048576，但那只是「prompt token」的名义值。
	// 实测 1167 条消息 + 183 个工具（约 126 万字符）会被上游 400 拒掉，
	// 因为工具 schema 在 token 化后的开销远大于字符数。
	// 这里保守降到 600k，宁可多裁也不让请求被拒。
	maxPromptTokens = 600_000
	// minKeepMessages 是无论怎么裁都要保留的最近消息条数。
	minKeepMessages = 20
	// maxAnchorMessages 是开头锚点最多保留几条用户消息。
	// 太少记不住任务目标，太多会挤掉近期上下文。
	maxAnchorMessages = 6
	// charsPerToken 是粗略换算；中文约 1.5 字/token，英文约 4 字符/token。
	// 原来取 3，实测会低估：agent 场景里大量是 JSON、工具调用等低信息密度文本，
	// 真实 token 数明显高于 char/3。取 2 更接近实际，且偏保守。
	charsPerToken = 2
	// toolsCharsPerToken 是工具 schema 的换算系数。
	// 工具描述里每个参数名、类型、enum 都要单独成 token，比普通文本更密，
	// 用更小的分母换算，等于给工具更高权重。
	toolsCharsPerToken = 1
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

	// 估算整体 token：消息与工具分开算，工具用更小的分母（权重更高）
	msgChars := 0
	for _, msg := range msgs {
		msgChars += len(msg)
	}
	toolsChars := 0
	if t, ok := m["tools"]; ok {
		toolsChars = len(t)
	}
	est := msgChars/charsPerToken + toolsChars/toolsCharsPerToken
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
	// toolsChars 在上面整体估算时已取过，这里复用
	budgetChars := (maxPromptTokens - 50_000) * charsPerToken // 再留 5万 token 给输出
	// 工具按更高权重折算成等效字符数，从预算里扣掉
	availChars := budgetChars - sysChars - toolsChars*charsPerToken/toolsCharsPerToken

	// 先取「开头锚点」：最早的若干条用户消息。
	//
	// 为什么要有这个：只保留最近 N 条会让模型「变笨」——用户一开始交代的
	// 目标、路径、约束全在被丢掉的部分里，模型只看得到最近几轮，
	// 表现就是记不住前面说过什么。保留开头能把这个锚留住。
	var anchors []json.RawMessage
	anchorChars := 0
	anchorBudget := availChars / 5 // 最多 1/5 预算给开头，不能挤掉近期上下文
	for _, msg := range rest {
		if len(anchors) >= maxAnchorMessages || anchorChars+len(msg) > anchorBudget {
			break
		}
		var head struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(msg, &head)
		// 锚点只保留用户消息与非空 assistant 文本，跳过工具调用链
		if head.Role != "user" {
			continue
		}
		anchors = append(anchors, msg)
		anchorChars += len(msg)
	}

	// 再从尾部往前累加，能放下多少就保留多少
	var kept []json.RawMessage
	acc := anchorChars
	for i := len(rest) - 1; i >= 0; i-- {
		n := len(rest[i])
		// 已被锚点选中的消息不重复放
		if i < len(anchors) {
			break
		}
		if acc+n > availChars && len(kept) >= minKeepMessages {
			break
		}
		kept = append([]json.RawMessage{rest[i]}, kept...)
		acc += n
	}

	// 锚点与近期窗口之间若确有消息被丢掉，插一条提示，
	// 让模型知道中间有缺失，而不是以为上下文是连续的。
	droppedBetween := len(rest) - len(anchors) - len(kept)
	var bridge []json.RawMessage
	if droppedBetween > 0 && len(anchors) > 0 && len(kept) > 0 {
		note := map[string]any{
			"role": "user",
			"content": fmt.Sprintf("[网关提示] 为节省上下文，中间的 %d 条历史已省略。"+
				"如需依据更早的细节，请重新说明。", droppedBetween),
		}
		if nb, err := json.Marshal(note); err == nil {
			bridge = append(bridge, nb)
		}
	}

	// 合并顺序：开头锚点 → 省略提示 → 最近窗口
	kept = append(append(append([]json.RawMessage{}, anchors...), bridge...), kept...)

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

	est2 := (sysChars+acc)/charsPerToken + toolsChars/toolsCharsPerToken
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
