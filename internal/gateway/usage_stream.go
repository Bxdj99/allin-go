// 本文件是「用量统计」的提取层：只负责从上游响应里把 token 数抠出来，
// 并把字节流原样转发给客户端（流式时边转边抽，不缓存整条响应）。
//
// 支持三种来源：
//   - OpenAI 非流式 JSON：body.usage.prompt_tokens / completion_tokens
//   - OpenAI 流式 SSE：最后一帧 data 里的 usage（需要请求带 stream_options.include_usage，
//     见 withStreamUsage 与 workbuddy.ForceStream）
//   - Anthropic 流式 SSE：message_start 的 message.usage（输入）+
//     message_delta 的 usage（输出，Anthropic 这里给的是累计值，直接覆盖）
//
// 拿不到就返回 0，调用方照样记请求数。
package gateway

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"time"
)

// usageTokens 是一次响应里的 token 用量与积分消耗。
type usageTokens struct {
	In    int
	Out   int
	Total int
	// Credit 是上游返回的本次消耗积分（WorkBuddy 会带这个字段，如 0.02）。
	// 用来做「哪个账号花了多少积分」的统计 —— 用户反馈积分莫名消失时，
	// 这是唯一能自证清白的证据来源。
	Credit float64

	// 缓存用量。三个 has 标记区分「明确 0」与「上游没给」——
	// 前者是有意义的结论（确实没命中），后者只能显示「—」。
	CacheRead     int64
	CacheWrite    int64
	hasCacheRead  bool
	hasCacheWrite bool
}

// add 把另一来源并入（同一项取较大值，兼容「部分帧只给增量」的上游）。
func (u *usageTokens) add(o usageTokens) {
	if o.In > u.In {
		u.In = o.In
	}
	if o.Out > u.Out {
		u.Out = o.Out
	}
	if o.Total > u.Total {
		u.Total = o.Total
	}
	// 积分是累计值，取最大（同一响应里可能多帧重复给）
	if o.Credit > u.Credit {
		u.Credit = o.Credit
	}
	// 缓存量同样是累计值：多帧重复给时取最大，
	// 但只有真的给过才算「知道」（has 标记不能让 0 覆盖掉 true）。
	if o.hasCacheRead {
		if !u.hasCacheRead || o.CacheRead > u.CacheRead {
			u.CacheRead = o.CacheRead
		}
		u.hasCacheRead = true
	}
	if o.hasCacheWrite {
		if !u.hasCacheWrite || o.CacheWrite > u.CacheWrite {
			u.CacheWrite = o.CacheWrite
		}
		u.hasCacheWrite = true
	}
}

// empty 判断是否什么都没抽到。
func (u usageTokens) empty() bool { return u.In == 0 && u.Out == 0 && u.Total == 0 }

// counts 转成落账用的计数；上游只给 total 时按输出算（DeepSeek 网页版就是这样）。
func (u usageTokens) counts() usageCounts {
	c := usageCounts{InTokens: u.In, OutTokens: u.Out}
	if c.InTokens == 0 && c.OutTokens == 0 && u.Total > 0 {
		c.OutTokens = u.Total
	}
	return c
}

// usageFromJSON 从一段非流式响应体里抽 usage（OpenAI 与 Anthropic 两种字段名都认）。
func usageFromJSON(raw []byte) usageTokens {
	var env struct {
		Usage *struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			TotalTokens      int     `json:"total_tokens"`
			InputTokens      int     `json:"input_tokens"`
			OutputTokens     int     `json:"output_tokens"`
			Credit           float64 `json:"credit"`
		} `json:"usage"`
		// Anthropic 的整条响应：usage 直接在顶层，偶尔也会被包在 message 里
		Message *struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return usageTokens{}
	}
	var u usageTokens
	if env.Usage != nil {
		u = usageTokens{
			In:     firstNonZero(env.Usage.PromptTokens, env.Usage.InputTokens),
			Out:    firstNonZero(env.Usage.CompletionTokens, env.Usage.OutputTokens),
			Total:  env.Usage.TotalTokens,
			Credit: env.Usage.Credit,
		}
	}
	if env.Message != nil && env.Message.Usage != nil {
		u.add(usageTokens{In: env.Message.Usage.InputTokens, Out: env.Message.Usage.OutputTokens})
	}
	attachCacheUsage(&u, raw)
	return u
}

// usageFromEvent 从一帧 SSE 的 data 里抽 usage。
//
// OpenAI：usage 出现在最后一帧（include_usage 时是 [DONE] 之前那一帧）。
// Anthropic：message_start 带 message.usage.input_tokens，
// message_delta 带 usage.output_tokens，且都是「到目前为止的累计值」，直接覆盖。
func usageFromEvent(data []byte) usageTokens {
	if len(data) == 0 || data[0] != '{' {
		return usageTokens{}
	}
	var env struct {
		Usage *struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			TotalTokens      int     `json:"total_tokens"`
			InputTokens      int     `json:"input_tokens"`
			OutputTokens     int     `json:"output_tokens"`
			CacheRead        int     `json:"cache_read_input_tokens"`
			CacheCreation    int     `json:"cache_creation_input_tokens"`
			Credit           float64 `json:"credit"`
		} `json:"usage"`
		Message *struct {
			Usage *struct {
				InputTokens   int `json:"input_tokens"`
				OutputTokens  int `json:"output_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &env) != nil {
		return usageTokens{}
	}
	var u usageTokens
	if env.Usage != nil {
		u = usageTokens{
			In:     firstNonZero(env.Usage.PromptTokens, env.Usage.InputTokens),
			Out:    firstNonZero(env.Usage.CompletionTokens, env.Usage.OutputTokens),
			Total:  env.Usage.TotalTokens,
			Credit: env.Usage.Credit,
		}
		if env.Usage.InputTokens > 0 {
			// Anthropic 的 input 不含缓存命中/写入部分，按总量补齐更接近计费口径
			u.In += env.Usage.CacheRead + env.Usage.CacheCreation
		}
	}
	if env.Message != nil && env.Message.Usage != nil {
		in := env.Message.Usage.InputTokens + env.Message.Usage.CacheRead + env.Message.Usage.CacheCreation
		u.add(usageTokens{In: in, Out: env.Message.Usage.OutputTokens})
		// Anthropic 形态：缓存量在上面已经加进 In 了，
		// 这里单独记一份原始值，供命中率计算用（分母要按含缓存的口径）。
		if env.Message.Usage.CacheRead > 0 {
			u.CacheRead, u.hasCacheRead = int64(env.Message.Usage.CacheRead), true
		}
		if env.Message.Usage.CacheCreation > 0 {
			u.CacheWrite, u.hasCacheWrite = int64(env.Message.Usage.CacheCreation), true
		}
	}
	// 统一走一遍完整字段表：覆盖 prompt_tokens_details.cached_tokens、
	// prompt_cache_hit_tokens 等 OpenAI 系别名，与上游 CacheUsage 口径一致。
	attachCacheUsage(&u, data)
	return u
}

// firstNonZero 取第一个非零值（OpenAI / Anthropic 字段名混用时兜底）。
func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// withStreamUsage 给 OpenAI 系的流式请求体补上 stream_options.include_usage=true。
//
// 客户端没要流式、或已经显式关掉了 include_usage，就不动它；
// 上游不认识这个字段会忽略，无害。只改发往上游的请求体，不改客户端看到的响应。
func withStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	streamRaw, ok := m["stream"]
	if !ok || string(bytes.TrimSpace(streamRaw)) != "true" {
		return body
	}
	if raw, has := m["stream_options"]; has {
		var opts map[string]json.RawMessage
		if json.Unmarshal(raw, &opts) == nil && len(opts) > 0 {
			if v, hasInc := opts["include_usage"]; hasInc && string(bytes.TrimSpace(v)) == "false" {
				return body // 客户端明确不要，尊重它
			}
			opts["include_usage"] = json.RawMessage(`true`)
			merged, err := json.Marshal(opts)
			if err != nil {
				return body
			}
			m["stream_options"] = merged
		} else {
			m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
	} else {
		m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// sseInspector 逐块扫 SSE 字节流，只维护「当前这一帧的 data 行」，不缓存正文。
//
// 写往客户端的每一块字节都过一遍 feed，结束时取 result。
// 一帧可能跨多次 read、一次 read 里也可能有多帧，所以按行推进，遇空行结算。
type sseInspector struct {
	line   []byte // 当前未完结的一行
	isData bool
	data   []byte // 当前帧的 data 缓冲（单帧 JSON 很小）
	usage  usageTokens
}

// feed 处理一块字节。
func (s *sseInspector) feed(chunk []byte) {
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			s.line = append(s.line, chunk...)
			return
		}
		s.line = append(s.line, chunk[:i]...)
		s.endLine()
		chunk = chunk[i+1:]
	}
}

// endLine 处理一行完整内容（不含 \n，可能带 \r）。
func (s *sseInspector) endLine() {
	line := bytes.TrimRight(s.line, "\r")
	s.line = s.line[:0]
	if len(line) == 0 {
		s.settle() // 空行 = 一帧结束
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		payload := bytes.TrimSpace(line[len("data:"):])
		if s.isData {
			s.data = append(s.data, '\n') // 多行 data 按 SSE 规范用 \n 拼接
		}
		s.data = append(s.data, payload...)
		s.isData = true
	}
}

// settle 结算一帧。
func (s *sseInspector) settle() {
	if s.isData && len(s.data) > 0 {
		if u := usageFromEvent(s.data); !u.empty() { // [DONE] 之类的非 JSON 会被跳过
			s.usage.add(u)
		}
	}
	s.data = s.data[:0]
	s.isData = false
}

// result 返回累计到的 usage（末尾没换行的半帧也结算掉）。
func (s *sseInspector) result() usageTokens {
	if s.isData || len(s.line) > 0 {
		s.endLine()
		s.settle()
	}
	return s.usage
}

// ---------- 请求级记账上下文 ----------

// usageCtx 是一次客户端请求的记账上下文。
//
// 为什么需要它：一条客户端请求在上游可能要换号、换模型、甚至换供应商（fallback.Chain），
// 只按「刚开始选中的那个供应商」记账会把降级后的真实用量记到错的 key 上。
// 所以 record 时可以把真实服务方传进来覆盖默认值。
//
// 每条**上游尝试**记一次；成功的那次带 token，失败的那次只记失败计数。
type usageCtx struct {
	providerID      string
	model           string
	defaultProvider string
	defaultModel    string
	started         time.Time
	done            bool

	// accountID 是本次实际使用的账号，用于按账号统计积分消耗。
	// 用户反馈「积分莫名少了」时，只能靠这个数据自证。
	accountID string
	// creditStats 表示是否记账号级积分统计（面板可关）。
	creditStats bool

	// onLoad 把本次真实 token 消耗喂给选号调度器。
	// 为 nil 表示不参与（例如单测或非账号池路径）。
	//
	// 为什么放在记账点：这里是唯一拿到「本次到底花了多少 token」的地方。
	// 选号调度器按它打分，才能把重活摊开 —— 只按请求次数轮转是轻重大小不分的。
	onLoad func(tokens float64)
}

// newUsageCtx 为一条客户端请求建记账上下文。
//
// origin 是客户端请求里声明的模型（例如 workbuddy/glm-5.3），
// 会作为「还没走到 relay 就失败」时的兜底 key。
func newUsageCtx(origin string) *usageCtx {
	pid, model := splitModelKey(origin)
	return &usageCtx{
		providerID:      pid,
		model:           model,
		defaultProvider: pid,
		defaultModel:    model,
		started:         time.Now(),
	}
}

// splitModelKey 把「供应商/模型」拆成两段；模型名里允许再出现 /。
func splitModelKey(key string) (string, string) {
	i := strings.Index(key, "/")
	if i < 0 {
		return "", key
	}
	return key[:i], key[i+1:]
}

// record 记一次上游尝试的最终结果。providerID/model 传空则用默认值。
//
// 同一 ctx 只允许记一次（先到先得），避免 relayXxx 里多处收尾重复记账。
func (c *usageCtx) record(usageProvider, usageModel string, ok bool, u usageTokens) {
	if c == nil || c.done {
		return
	}
	c.done = true
	if usageProvider == "" {
		usageProvider = c.defaultProvider
	}
	if usageModel == "" {
		usageModel = c.defaultModel
	}
	recordUsageStat(usageProvider, usageModel, ok, u.counts())
	if c.creditStats && c.accountID != "" {
		recordAccountUsage(usageProvider, c.accountID, ok, u)
	}
	// 缓存命中写进日志：这是判断「省积分改动有没有效果」最直接的证据。
	// 只在成功且上游报过缓存字段时打，避免刷无意义的行。
	if ok && u.HasCacheInfo() {
		rate, known := u.CacheHitRate()
		if known {
			log.Printf("[CACHE] acct=%s in=%d cached=%d write=%d hit=%.1f%%",
				c.accountID, u.In, u.CacheRead, u.CacheWrite, rate*100)
		}
	}

	// 把真实 token 数喂给选号调度器。
	// 失败也要报（传 0），否则在途名额不释放，那个号会被永久低估。
	if c.onLoad != nil {
		if ok {
			c.onLoad(float64(u.In + u.Out))
		} else {
			c.onLoad(0)
		}
	}
}

// fail 记一次失败（没有 token）。providerID/model 传空则用默认值；传一个参数时两者都取默认。
func (c *usageCtx) fail(usageProviderAndModel ...string) {
	var p, m string
	if len(usageProviderAndModel) >= 2 {
		p, m = usageProviderAndModel[0], usageProviderAndModel[1]
	}
	c.record(p, m, false, usageTokens{})
}
