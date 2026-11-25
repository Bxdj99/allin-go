package workbuddy

import (
	"encoding/json"
	"fmt"
	"sort"
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
	// maxPromptTokens 是裁剪阈值。上游硬上限 1048576，这里留约 10% 余量取 950k。
	//
	// 为什么不更保守：用户明确要求「别裁太狠，跟上上游上限差不多就行」。
	// 裁得越狠，模型越容易忘掉前面交代的东西（表现为「变笨」），
	// 而且长上下文通常命中 prompt cache，实际花费远低于名义 token 数。
	//
	// 为什么不贴着 1048576：估算用 char/2 + 工具单独系数，本身有误差，
	// 留 10% 缓冲避免因低估而被上游 400 拒。
	// 真被拒也有兜底：server.go 里会按 2/3 逐轮收紧预算重发。
	maxPromptTokens = 950_000
	// maxTruncateRounds 是「丢整条」之前最多做几轮渐进截断。
	maxTruncateRounds = 30
	// minTruncatableChars 低于这个长度的消息不值得截断。
	minTruncatableChars = 400
	// minKeepMessages 是无论怎么裁都要保留的最近消息条数。
	// 20 条对长对话偏少，会让刚发生的几轮之外全被切掉；
	// 提到 40 条，配合更高的阈值，尽量少动上下文。
	minKeepMessages = 40
	// maxAnchorMessages 是开头锚点最多保留几条用户消息。
	// 太少记不住任务目标，太多会挤掉近期上下文。
	maxAnchorMessages = 6
	// charsPerToken 仅用于「把 token 预算折成字符预算」这类粗略换算。
	// 真正算 token 用 estimateTextTokens（区分中日韩与其余字符）。
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
	Body      []byte
	Dropped   int // 丢掉的消息条数
	Estimated int // 裁剪后的估算 token
	Trimmed   bool

	// Before 是裁剪前的估算 token，Before-BP 就是这次裁剪省下的量。
	// 之所以要记它：用户问「积分为什么这么高」时，
	// 「裁剪到底省了多少」是必须能回答的问题 —— 之前完全看不到。
	Before int
}

// TrimContext 检查并裁剪请求体里的 messages，使其不超过上游上下文上限。
// TrimContext 用默认阈值裁剪（等价于 TrimContextTo(body, 0)）。
func TrimContext(body []byte) (*TrimResult, error) {
	return TrimContextTo(body, 0)
}

// TrimContextTo 把请求裁剪到指定 token 预算内。budget<=0 用默认阈值。
//
// 渐进式设计：网关第一次按「默认阈值」裁，如果上游仍然嫌大，
// 由调用方用更小的 budget 再来一次。这样：
//   - 大多数请求只裁一次，代价最小
//   - 图片、长文本不会被无谓地砍掉
//   - 只有真正超限时才逐级收紧
func TrimContextTo(body []byte, budget int) (*TrimResult, error) {
	limit := maxPromptTokens
	if budget > 0 && budget < limit {
		limit = budget
	}
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
	// 逐条按 CJK 规则估算，工具 schema 用更小分母（权重更高）
	msgTokens := 0
	for _, msg := range msgs {
		msgTokens += estimateRawTokens(msg)
	}
	toolsChars := 0
	if t, ok := m["tools"]; ok {
		toolsChars = len(t)
	}
	est := msgTokens + toolsChars/toolsCharsPerToken
	if est <= limit {
		return &TrimResult{Body: body, Estimated: est, Before: est}, nil
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
	budgetChars := (limit - 50_000) * charsPerToken // 再留 5万 token 给输出
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

	// 先挑出「必须保留」的消息：
	//   1. 含图片的消息 —— 用户发图就是要模型看图，裁掉等于答非所问
	//   2. 最新一条 user —— 这是本次请求的核心问题
	//
	// 它们即使超预算也要保留；否则裁到最后模型连问题都看不到。
	mandatory := make(map[int]bool)
	latestUser := -1
	for i := len(rest) - 1; i >= 0; i-- {
		var head struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(rest[i], &head) != nil {
			continue
		}
		if head.Role == "user" && latestUser < 0 {
			latestUser = i
		}
		if hasImageContent(head.Content) {
			mandatory[i] = true
		}
	}
	if latestUser >= 0 {
		mandatory[latestUser] = true
	}

	// 再从尾部往前累加，能放下多少就保留多少；强制保留的忽略预算
	var keptIdx []int
	acc := anchorChars
	for i := len(rest) - 1; i >= 0; i-- {
		n := len(rest[i])
		// 已被锚点选中的消息不重复放
		if i < len(anchors) {
			break
		}
		if !mandatory[i] && acc+n > availChars && len(keptIdx) >= minKeepMessages {
			break
		}
		keptIdx = append(keptIdx, i)
		acc += n
	}
	// 把强制保留但没被窗口圈进来的消息补上
	inWindow := make(map[int]bool, len(keptIdx))
	for _, i := range keptIdx {
		inWindow[i] = true
	}
	for i := range mandatory {
		if !inWindow[i] && (len(anchors) == 0 || i >= len(anchors)) {
			keptIdx = append(keptIdx, i)
			acc += len(rest[i])
		}
	}
	sort.Sort(sort.IntSlice(keptIdx))

	var kept []json.RawMessage
	for _, i := range keptIdx {
		kept = append(kept, rest[i])
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

	// 修复 tool 配对。
	//
	// 上游对这块极严，配不上就直接 400 code=11148：
	//   "tool calls and tool results do not match"
	//
	// 裁剪会从两个方向破坏配对：
	//   1) assistant(tool_calls) 被保留，但它后面的 tool 结果被切掉
	//   2) tool 结果被保留，但前面的 assistant(tool_calls) 被切掉
	//   3) 锚点与近期窗口拼接处，一组调用被拦腰截断
	//
	// 统一处理：遍历一遍，只保留「调用与结果双双存在」的完整组。
	// 任何残缺的 tool 消息或悬空的 tool_calls 一律清理掉。
	kept = sanitizeToolPairs(kept)

	// 单条巨型消息兜底。
	//
	// 按条数裁剪挡不住「一条消息就几十万字」的情况：agent 把整个文件、
	// 整页日志塞进一条 user/tool 消息里很常见。实测出现过单条 1.8MB，
	// 占 90 万 token，裁剪后仍然超限，上游直接 400。
	//
	// 对这类消息做内容截断，保留开头与结尾，中间标注省略长度。
	maxMsgChars := limit * charsPerToken / 2 // 单条最多占一半预算
	for i, raw := range kept {
		if len(raw) <= maxMsgChars {
			continue
		}
		if shrunk, ok := shrinkMessage(raw, maxMsgChars); ok {
			kept[i] = shrunk
		}
	}

	// 渐进截断：仍超预算就反复截断「当前最长的那条可截断文本」。
	//
	// 这是「丢整条」之前的最后一层缓冲。区别很大：
	//   - 截断只丢内容尾部，消息本身还在，模型知道这里发生过什么
	//   - 丢整条会让中间的对话直接消失
	// 所以优先截断，实在不行才丢。
	//
	// system 与含图片的消息不参与截断（前者行为依赖，后者截了就没意义）。
	for round := 0; round < maxTruncateRounds; round++ {
		if trimmedEstimate(kept, toolsChars) <= limit {
			break
		}
		longestIdx, longestLen := -1, 0
		for i, raw := range kept {
			if isProtectedFromTruncate(raw) {
				continue
			}
			n := len(raw)
			if n > longestLen && n > minTruncatableChars {
				longestIdx, longestLen = i, n
			}
		}
		if longestIdx < 0 {
			break
		}
		// 每次砍到当前长度的一半，至少留 64 字符
		target := longestLen / 2
		if shrunk, ok := shrinkMessage(kept[longestIdx], target); ok {
			kept[longestIdx] = shrunk
			continue
		}
		break
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

	// 用最终产物重新估算：上面做了配对清理、单条截断、图片限流，
	// 这些都会改变实际体积，沿用裁剪时的 acc 会高估。
	finalMsgs := 0
	for _, raw := range kept {
		finalMsgs += estimateRawTokens(raw)
	}
	est2 := finalMsgs + toolsChars/toolsCharsPerToken
	return &TrimResult{
		Body:      nb,
		Dropped:   len(rest) - len(kept),
		Estimated: est2,
		Before:    est,
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

// sanitizeToolPairs 确保 assistant.tool_calls 与后续 tool 消息严格配对。
//
// 规则：
//   - assistant 声明了 tool_calls，就必须紧跟其全部对应的 tool 消息；
//     缺任何一条 → 整个调用声明与已出现的部分结果一起丢掉
//   - 没有任何 assistant 声明过的 tool 消息 → 丢掉
//   - 非 tool 消息不参与配对，原样保留
//
// 返回新切片，不修改入参。
func sanitizeToolPairs(msgs []json.RawMessage) []json.RawMessage {
	type parsed struct {
		role    string
		callIDs []string // assistant 的 tool_calls id 列表
		toolID  string   // tool 消息对应的 id
		raw     json.RawMessage
	}
	items := make([]parsed, len(msgs))
	for i, raw := range msgs {
		var p parsed
		p.raw = raw
		var m struct {
			Role      string          `json:"role"`
			ToolCalls json.RawMessage `json:"tool_calls"`
			ToolID    string          `json:"tool_call_id"`
		}
		if json.Unmarshal(raw, &m) == nil {
			p.role = m.Role
			p.toolID = m.ToolID
			if len(m.ToolCalls) > 0 {
				var calls []struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(m.ToolCalls, &calls) == nil {
					for _, c := range calls {
						p.callIDs = append(p.callIDs, c.ID)
					}
				}
			}
		}
		items[i] = p
	}

	// 先算出每个 tool_call id 出现了几次、被声明过几次
	declared := map[string]bool{}
	for _, it := range items {
		for _, id := range it.callIDs {
			declared[id] = true
		}
	}
	answered := map[string]bool{}
	for _, it := range items {
		if it.role == "tool" && it.toolID != "" {
			answered[it.toolID] = true
		}
	}

	out := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		switch it.role {
		case "tool":
			// 结果必须有对应的调用声明，否则是孤立的
			if it.toolID == "" || !declared[it.toolID] {
				continue
			}
			out = append(out, it.raw)
		default:
			if len(it.callIDs) == 0 {
				out = append(out, it.raw)
				continue
			}
			// 声明了调用：要求每个 id 都有结果，否则整条声明作废
			complete := true
			for _, id := range it.callIDs {
				if !answered[id] {
					complete = false
					break
				}
			}
			if complete {
				out = append(out, it.raw)
			}
			// 不完整就丢掉这条 assistant 消息；
			// 它的 tool 结果会在后面因 declared 为空而被一并丢弃
			if !complete {
				for _, id := range it.callIDs {
					declared[id] = false
				}
			}
		}
	}

	// 第二轮：上面丢弃声明后，某些 tool 结果失去了归属，再筛一次
	final := make([]json.RawMessage, 0, len(out))
	live := map[string]bool{}
	for _, raw := range out {
		var m struct {
			ToolCalls json.RawMessage `json:"tool_calls"`
		}
		if json.Unmarshal(raw, &m) == nil && len(m.ToolCalls) > 0 {
			var calls []struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(m.ToolCalls, &calls) == nil {
				for _, c := range calls {
					live[c.ID] = true
				}
			}
		}
	}
	for _, raw := range out {
		var m struct {
			Role   string `json:"role"`
			ToolID string `json:"tool_call_id"`
		}
		if json.Unmarshal(raw, &m) == nil && m.Role == "tool" {
			if m.ToolID == "" || !live[m.ToolID] {
				continue
			}
		}
		final = append(final, raw)
	}
	return final
}

// shrinkMessage 把单条超大消息截断到 limit 字节以内。
//
// 保留头尾各一半，中间替换成一行说明。为什么保留结尾：
// 用户最新问题、报错堆栈的最后一行往往在末尾，全砍掉会让模型答非所问。
//
// 支持 OpenAI 的两种 content 形态（字符串 / block 数组）与 tool 消息。
// 返回 ok=false 表示无法安全截断（结构不认识），调用方保持原样。
func shrinkMessage(raw json.RawMessage, limit int) (json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw, false
	}
	craw, ok := m["content"]
	if !ok {
		return raw, false
	}

	var text string
	var isArray bool
	if json.Unmarshal(craw, &text) != nil {
		// 尝试 block 数组：只处理纯文本块，其它类型不冒险
		var parts []map[string]json.RawMessage
		if json.Unmarshal(craw, &parts) != nil {
			return raw, false
		}
		isArray = true
		var joined string
		for _, p := range parts {
			var t string
			if json.Unmarshal(p["text"], &t) == nil {
				joined += t
			}
		}
		text = joined
	}

	if len(text) <= limit {
		return raw, false
	}

	half := limit / 2
	if half < 64 {
		half = 64
	}
	if half*2 >= len(text) {
		return raw, false
	}
	head, tail := text[:half], text[len(text)-half:]
	omitted := len(text) - half*2
	shrunk := head + fmt.Sprintf("\n\n[网关截断] 此处省略 %d 字符（单条消息过长）\n\n", omitted) + tail

	var buf []byte
	var err error
	if isArray {
		buf, err = json.Marshal([]map[string]any{{"type": "text", "text": shrunk}})
	} else {
		buf, err = json.Marshal(shrunk)
	}
	if err != nil {
		return raw, false
	}
	m["content"] = buf
	out, err := json.Marshal(m)
	if err != nil {
		return raw, false
	}
	return out, true
}

// trimmedEstimate 按当前消息数组估算 token。
func trimmedEstimate(msgs []json.RawMessage, toolsChars int) int {
	total := 0
	for _, raw := range msgs {
		total += estimateRawTokens(raw)
	}
	return total + toolsChars/toolsCharsPerToken
}

// DefaultPromptBudget 返回默认裁剪阈值，供网关做渐进式收紧的起点。
func DefaultPromptBudget() int { return maxPromptTokens }

// estimateTextTokens 估算一段文本的 token 数。
//
// 中日韩字符按 1 字符 1 token，其余按 4 字符 1 token。
// 这比「一律 char/2」准得多：
//   - 中文在 char/2 下被低估一半 → 裁剪该触发时不触发 → 上游 400
//   - 英文在 char/2 下被高估一倍 → 明明没超也裁掉 → 模型丢上下文
//
// 阈值 0x2E80 覆盖 CJK 部首、汉字、假名、韩文等区段。
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// estimateRawTokens 估算一段原始 JSON 的 token 数（含引号与键名）。
func estimateRawTokens(raw []byte) int {
	return estimateTextTokens(string(raw))
}

// hasImageContent 判断一条消息的 content 里是否含图片块。
//
// OpenAI 形态是 [{"type":"image_url",...}]，Anthropic 是 [{"type":"image",...}]，
// 两种都认。
func hasImageContent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	// 字符串形态一定没有图片
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return false
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "image_url" || b.Type == "image" {
			return true
		}
	}
	return false
}

// isProtectedFromTruncate 判断一条消息是否禁止内容截断。
//
// system / developer 消息是行为指令，截了模型就变了；
// 含图片的消息截文本没意义（体积在 base64 里）。
func isProtectedFromTruncate(raw json.RawMessage) bool {
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return true
	}
	if m.Role == "system" || m.Role == "developer" {
		return true
	}
	return hasImageContent(m.Content)
}

// CapMaxTokens 把请求里的输出上限封顶。
//
// 只下调不上调：客户端要 100、上限 4096 时保持 100 不变；
// 客户端要 100000、上限 4096 时改为 4096。
// 目的是防止客户端传一个离谱的值导致上游拒单或费用失控。
//
// 同时处理 max_tokens 与 max_completion_tokens 两种字段名。
func CapMaxTokens(body []byte, cap int) []byte {
	if cap <= 0 || len(body) == 0 {
		return body
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	changed := false
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var v float64
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		if v > float64(cap) {
			buf, err := json.Marshal(cap)
			if err != nil {
				continue
			}
			m[k] = buf
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
