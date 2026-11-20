package workbuddy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 历史压缩。
//
// 动机：agent 会话里，早期的大量工具调用（读文件、跑命令）占据绝大部分上下文，
// 但它们对「当前这一步」几乎没用了 —— 模型真正需要的是结论，不是每一条原始输出。
//
// 与裁剪的区别：
//   - 裁剪（TrimContextTo）是超限时才丢消息，丢的是整条
//   - 压缩是主动把「早期的工具调用组」换成一句摘要，保留语义轮廓
//
// 安全约束（这个上游对配对极严，11148 就是配对断裂）：
//   - 以「工具组」为单位处理：assistant(tool_calls) + 它的全部 tool 结果，
//     要么整组压缩，要么整组保留，绝不拆开
//   - 最近 N 组永远不压缩（当前任务上下文）
//   - 每个 tool_call_id 在压缩后要么仍有对应 tool 消息，要么调用声明一起去掉
//   - 压缩后仍走一遍 sanitizeToolPairs 做最终校验

// CompressOptions 控制压缩力度。
type CompressOptions struct {
	// KeepRecentGroups 保留最近多少组工具调用不压缩。
	KeepRecentGroups int
	// MinGroupChars 低于这个字符数的组不值得压缩（省不下什么，还丢信息）。
	MinGroupChars int
}

func defaultCompressOptions() CompressOptions {
	return CompressOptions{KeepRecentGroups: 6, MinGroupChars: 2000}
}

// CompressHistory 把较早的工具调用组替换成摘要行。
//
// 返回压缩后的请求体；未发生压缩时原样返回。
func CompressHistory(body []byte) ([]byte, int) {
	return CompressHistoryWith(body, defaultCompressOptions())
}

// CompressHistoryWith 是带参数的版本。
func CompressHistoryWith(body []byte, opts CompressOptions) ([]byte, int) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, 0
	}
	raw, ok := m["messages"]
	if !ok {
		return body, 0
	}
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return body, 0
	}

	// 1) 把所有消息切成「组」。
	//    一组 = 一条 assistant(带 tool_calls) + 紧随其后的全部 tool 消息。
	//    其它消息各自成组（普通对话、system 等）。
	type group struct {
		msgs    []json.RawMessage
		isTools bool // 是否是工具调用组
		chars   int
	}
	var groups []group
	i := 0
	for i < len(msgs) {
		cur := msgs[i]
		var head struct {
			Role      string          `json:"role"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		}
		json.Unmarshal(cur, &head)

		if head.Role == "assistant" && len(head.ToolCalls) > 2 {
			// 收集这一组：assistant + 后续 tool 消息
			g := group{isTools: true}
			g.msgs = append(g.msgs, cur)
			g.chars = len(cur)
			j := i + 1
			for j < len(msgs) {
				var r struct {
					Role string `json:"role"`
				}
				json.Unmarshal(msgs[j], &r)
				if r.Role != "tool" {
					break
				}
				g.msgs = append(g.msgs, msgs[j])
				g.chars += len(msgs[j])
				j++
			}
			groups = append(groups, g)
			i = j
			continue
		}
		groups = append(groups, group{msgs: []json.RawMessage{cur}, chars: len(cur)})
		i++
	}

	// 2) 找出可压缩的工具组：除了最近 KeepRecentGroups 组之外的。
	toolIdx := []int{}
	for idx, g := range groups {
		if g.isTools {
			toolIdx = append(toolIdx, idx)
		}
	}
	if len(toolIdx) <= opts.KeepRecentGroups {
		return body, 0
	}
	compressible := toolIdx[:len(toolIdx)-opts.KeepRecentGroups]

	// 3) 逐组替换成摘要。
	var rebuilt []json.RawMessage
	compressed := 0
	cs := map[int]bool{}
	for _, idx := range compressible {
		cs[idx] = true
	}

	// 预算剩余量，避免摘要本身把上下文撑大
	var droppedChars, summaryChars int
	for idx, g := range groups {
		if !cs[idx] || g.chars < opts.MinGroupChars {
			if cs[idx] {
				// 太小，不值得压缩，原样保留
				rebuilt = append(rebuilt, g.msgs...)
			} else {
				rebuilt = append(rebuilt, g.msgs...)
			}
			continue
		}
		summary := summarizeToolGroup(g.msgs)
		if summary == nil {
			rebuilt = append(rebuilt, g.msgs...)
			continue
		}
		rebuilt = append(rebuilt, summary)
		compressed++
		droppedChars += g.chars
		summaryChars += len(summary)
	}

	if compressed == 0 {
		return body, 0
	}

	// 4) 最终配对校验：压缩后不允许有孤立 tool 或悬空 tool_calls
	rebuilt = sanitizeToolPairs(rebuilt)

	buf, err := json.Marshal(rebuilt)
	if err != nil {
		return body, 0
	}
	m["messages"] = buf
	res, err := json.Marshal(m)
	if err != nil {
		return body, 0
	}
	return res, compressed
}

// summarizeToolGroup 把一组工具调用压成一条 assistant 摘要。
//
// 输出形态：一条 role=assistant 的普通消息，content 描述「调用了哪些工具、结果如何」。
// 不含 tool_calls，因此不会留下悬空的调用声明。
func summarizeToolGroup(msgs []json.RawMessage) json.RawMessage {
	if len(msgs) == 0 {
		return nil
	}
	var asst struct {
		ToolCalls []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(msgs[0], &asst) != nil {
		return nil
	}
	if len(asst.ToolCalls) == 0 {
		return nil
	}

	// 统计调用了哪些工具
	nameCount := map[string]int{}
	var names []string
	for _, c := range asst.ToolCalls {
		n := c.Function.Name
		if n == "" {
			n = "unknown"
		}
		if nameCount[n] == 0 {
			names = append(names, n)
		}
		nameCount[n]++
	}

	// 统计结果：成功条数与总字符
	toolCount := 0
	totalChars := 0
	for _, raw := range msgs[1:] {
		var t struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &t) != nil || t.Role != "tool" {
			continue
		}
		toolCount++
		totalChars += len(t.Content)
	}

	var parts []string
	for _, n := range names {
		if c := nameCount[n]; c > 1 {
			parts = append(parts, fmt.Sprintf("%s×%d", n, c))
		} else {
			parts = append(parts, n)
		}
	}

	content := fmt.Sprintf("[已压缩 %d 次工具调用：%s，返回约 %d 字。原始内容已省略]",
		toolCount, strings.Join(parts, "、"), totalChars)

	out := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return buf
}
