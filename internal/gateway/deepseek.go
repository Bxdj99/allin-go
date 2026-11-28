// DeepSeek 网页版（chat.deepseek.com）接入。
//
// 它和 OpenAI 兼容上游有三处根本差别，所以不能走通用透传，本文件专门处理：
//
//  1. 端点只有 /api/v0/chat/completion，每次请求前还得先领 PoW 挑战、
//     解出来后塞进 x-ds-pow-response 头（算法细节在 deepseekweb 包）；
//  2. 对话状态存在服务端，靠 chat_session_id + parent_message_id 把多轮串起来，
//     不是无状态的 messages 数组，所以网关必须自己缓存这两个 id；
//  3. 响应是 SSE patch 语义（{"p":"response/fragments/0/content","v":"…","o":"APPEND"}），
//     要按补丁自己拼文本，再转成标准 OpenAI chunk（或聚合后一次返回）。
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"allin/internal/bridge"
	"allin/internal/config"
	"allin/internal/provider"
	"allin/internal/provider/deepseekweb"
)

const (
	// dsSessionTTL 会话缓存有效期：超过这个时长没有新一轮就丢掉，下次请求重建会话。
	dsSessionTTL = 30 * time.Minute
	// dsMaxSessions 条目上限，超过时顺手清一遍过期项，避免长时间运行无界增长。
	dsMaxSessions = 512
	// dsPowTimeout 单次 PoW 求解上限；网页版自己给的窗口是 30 秒。
	dsPowTimeout = 30 * time.Second
)

// ---------------------------------------------------------------------------
// 会话缓存：按「供应商 + 账号 + 客户端会话标识」缓存 chat_session_id / parent_message_id
// ---------------------------------------------------------------------------

// dsSession 是一条上游会话链的运行时状态（不落盘）。
//
// turn 是容量 1 的信号量：同一条会话链上的请求必须一轮一轮来，
// 否则两个并发请求会拿着同一个 parent_message_id 各发一轮，把对话链岔开。
type dsSession struct {
	turn      chan struct{}
	sessionID string
	parentID  *int64
	updated   time.Time
	fails     int
}

// dsSessionStore 是会话缓存的容器。
type dsSessionStore struct {
	mu sync.Mutex
	m  map[string]*dsSession
}

// dsSessions 是进程内单例。
var dsSessions = &dsSessionStore{m: make(map[string]*dsSession)}

// lookup 取出未过期的条目，没有就新建（只碰 store 锁，不阻塞在会话上）。
func (st *dsSessionStore) lookup(key string) *dsSession {
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	if d, ok := st.m[key]; ok && now.Sub(d.updated) <= dsSessionTTL {
		return d
	}
	d := &dsSession{turn: make(chan struct{}, 1), updated: now}
	st.m[key] = d
	if len(st.m) > dsMaxSessions {
		for k, v := range st.m {
			if now.Sub(v.updated) > dsSessionTTL {
				delete(st.m, k)
			}
		}
	}
	return d
}

// acquire 取一条会话并占住它，直到 release。等待时可被请求的 ctx 取消，
// 免得客户端已经断开、goroutine 还卡在排队上。
func (st *dsSessionStore) acquire(ctx context.Context, key string) (*dsSession, error) {
	d := st.lookup(key)
	select {
	case d.turn <- struct{}{}:
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// release 释放会话并刷新活跃时间（updated 统一在 store 锁里改，避免和 lookup 竞争）。
func (st *dsSessionStore) release(key string, d *dsSession) {
	st.mu.Lock()
	if cur, ok := st.m[key]; ok && cur == d {
		d.updated = time.Now()
	}
	st.mu.Unlock()
	<-d.turn
}

// ids 返回本轮可复用的上游 id（调用方已占住会话）。
func (d *dsSession) ids() (string, *int64) {
	return d.sessionID, d.parentID
}

// commit 记下本轮成功后的会话链。没有 message_id 时保留原 parent，
// 只是把 session 换成本轮实际用的那个。
func (d *dsSession) commit(sessionID string, messageID int64, hasMessageID bool) {
	d.sessionID = sessionID
	if hasMessageID {
		id := messageID
		d.parentID = &id
	}
	d.fails = 0
}

// fail 记一次失败。连续两次失败就丢掉整条会话链：上游把会话清掉、
// 或者我们缓存了个已被删的 session 时，靠这个自愈，下一轮重建会话。
func (d *dsSession) fail() {
	d.fails++
	if d.fails >= 2 {
		d.sessionID = ""
		d.parentID = nil
		d.fails = 0
	}
}

// dsConvoKey 算「客户端会话标识」，决定哪些请求共用一条上游会话。
//
// 优先级：prompt_cache_key（OpenAI 生态里就是用来标会话的）→ user 字段 →
// messages 的哈希。前两个缺省时按 messages 内容哈希，同一段对话会命中同一条缓存。
func dsConvoKey(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		for _, k := range []string{"prompt_cache_key", "user"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return "n:" + strings.TrimSpace(v)
			}
		}
		if msgs, ok := m["messages"]; ok {
			if raw, err := json.Marshal(msgs); err == nil {
				sum := sha256.Sum256(raw)
				return "h:" + hex.EncodeToString(sum[:16])
			}
		}
	}
	sum := sha256.Sum256(body)
	return "h:" + hex.EncodeToString(sum[:16])
}

// dsModelSupported 判断模型名是否就是网页版支持的模式别名。
func dsModelSupported(name string) bool {
	for _, m := range deepseekweb.Models {
		if strings.EqualFold(m, name) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 响应回写：patch 增量 → OpenAI chunk（流式）或一次性 chat.completion（非流式）
// ---------------------------------------------------------------------------

// dsRelay 负责把上游增量写给客户端。
//
// 响应头是懒写的：在第一个增量到来之前仍是可改状态，这时上游出错还能换成正常的
// HTTP 错误码 + JSON；一旦开始写流，就只能断流了。
type dsRelay struct {
	w       http.ResponseWriter
	flusher http.Flusher
	stream  bool
	proto   protocol
	id      string
	model   string
	created int64
	started bool
}

func newDSRelay(w http.ResponseWriter, stream bool, proto protocol, model string) *dsRelay {
	flusher, _ := w.(http.Flusher)
	return &dsRelay{
		w: w, flusher: flusher, stream: stream, proto: proto,
		id: newDSChunkID(), model: model, created: time.Now().Unix(),
	}
}

// begin 首次写响应。流式时先发一个只带 role 的空增量，客户端好立刻开消息。
func (r *dsRelay) begin() error {
	if r.started {
		return nil
	}
	r.started = true
	if !r.stream {
		return nil
	}
	w := r.w
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return r.chunk(map[string]any{"role": "assistant", "content": ""}, nil)
}

// chunk 写一帧 OpenAI chat.completion.chunk。
func (r *dsRelay) chunk(delta map[string]any, finish any) error {
	payload := map[string]any{
		"id":      r.id,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []map[string]any{
			{"index": 0, "delta": delta, "finish_reason": finish},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(r.w, "data: %s\n\n", b); err != nil {
		return err
	}
	if r.flusher != nil {
		r.flusher.Flush()
	}
	return nil
}

// onText 是正文增量回调（fragment type = RESPONSE / TEMPLATE_RESPONSE）。
func (r *dsRelay) onText(delta string) error {
	if delta == "" {
		return nil
	}
	if err := r.begin(); err != nil {
		return err
	}
	return r.chunk(map[string]any{"content": delta}, nil)
}

// onReasoning 是思考链增量回调（THINK / REASONING / SEARCH / TOOL_*）。
// OpenAI 生态里这段放 reasoning_content，客户端的「思考过程」面板就吃这个字段。
func (r *dsRelay) onReasoning(delta string) error {
	if delta == "" {
		return nil
	}
	if err := r.begin(); err != nil {
		return err
	}
	return r.chunk(map[string]any{"reasoning_content": delta}, nil)
}

// finish 收尾：流式补 finish_reason 和 [DONE]，非流式把累积文本拼成一个 chat.completion。
func (r *dsRelay) finish(st *deepseekweb.StreamState) error {
	if r.stream {
		if err := r.begin(); err != nil {
			return err
		}
		if err := r.chunk(map[string]any{}, "stop"); err != nil {
			return err
		}
		if _, err := io.WriteString(r.w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if r.flusher != nil {
			r.flusher.Flush()
		}
		return nil
	}

	msg := map[string]any{"role": "assistant", "content": st.Text.String()}
	if st.Reasoning.Len() > 0 {
		msg["reasoning_content"] = st.Reasoning.String()
	}
	out := map[string]any{
		"id":      r.id,
		"object":  "chat.completion",
		"created": r.created,
		"model":   r.model,
		"choices": []map[string]any{
			{"index": 0, "message": msg, "finish_reason": "stop"},
		},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      st.Usage,
		},
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if r.proto == protocolAnthropic {
		raw, err = bridge.OpenAIToAnthropic(raw, r.model)
		if err != nil {
			return err
		}
	}
	r.started = true
	r.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	r.w.WriteHeader(http.StatusOK)
	_, err = r.w.Write(raw)
	return err
}

// newDSChunkID 生成 OpenAI 风格的响应 id。
func newDSChunkID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(buf)
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

// dsRetryable 判断这个上游错误值不值得换号重试。
// 凭据失效 / 限流 / 上游 5xx / 纯网络错误都可以换号；其余（模型不支持、PoW 解不出、
// 请求结构非法等）换号也没用。
func dsRetryable(err error) bool {
	var ue *deepseekweb.UpstreamError
	if errors.As(err, &ue) {
		if ue.UpstreamStatus == http.StatusUnauthorized || ue.UpstreamStatus == http.StatusForbidden ||
			ue.UpstreamStatus == http.StatusTooManyRequests || ue.UpstreamStatus >= 500 {
			return true
		}
		return false
	}
	// 非 UpstreamError：建会话 / 领挑战阶段的网络错误，换号通常也无害
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, deepseekweb.ErrPowTimeout) || errors.Is(err, deepseekweb.ErrPowUnsolved) {
		return false
	}
	return true
}

// dsGatewayStatus 把上游错误映射成给客户端的 HTTP 状态码。
func dsGatewayStatus(err error) int {
	var ue *deepseekweb.UpstreamError
	if errors.As(err, &ue) && ue.GatewayStatus > 0 {
		return ue.GatewayStatus
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return http.StatusBadGateway
}

// forwardDeepSeek 处理 kind = deepseek-web 的请求：
// 修模型别名 → 校验 → 取会话 → 一轮（PoW + completion）→ patch 流转 OpenAI 响应。
func (s *Server) forwardDeepSeek(w http.ResponseWriter, r *http.Request, p *config.Provider, realModel string, body []byte, stream bool, proto protocol, uc *usageCtx) {
	// Anthropic 客户端先把请求体翻成 OpenAI 形态。网页版没有流式输入，
	// 跨协议流式沿用其它供应商的口径：直接拒绝，让调用方改 stream=false。
	if proto == protocolAnthropic {
		if stream {
			uc.fail(p.ID, realModel)
			s.fail(w, http.StatusBadRequest,
				"DeepSeek 网页版暂不支持 Anthropic 流式请求，请把 stream 设为 false")
			return
		}
		converted, err := bridge.AnthropicToOpenAI(body, realModel)
		if err != nil {
			uc.fail(p.ID, realModel)
			s.fail(w, http.StatusBadRequest, "协议转换失败: "+err.Error())
			return
		}
		body = converted
	}

	// 网页版只认 deepseek-chat / deepseek-reasoner 两个「模式别名」，
	// 供应商里配了别的名字（比如 deepseek-web）就落到普通对话。
	alias := realModel
	if !dsModelSupported(alias) {
		alias = deepseekweb.ModelChat
	}
	rewritten, err := rewriteModel(body, alias)
	if err != nil {
		uc.fail(p.ID, realModel)
		s.fail(w, http.StatusInternalServerError, "改写请求体失败: "+err.Error())
		return
	}
	body = rewritten

	// 先做纯本地校验（模型、messages 结构、tools 形态），不合法立刻 400，
	// 别白跑一次 PoW 和建会话。
	if _, perr := deepseekweb.Prepare(body, "", nil); perr != nil {
		uc.fail(p.ID, realModel)
		s.fail(w, http.StatusBadRequest, perr.Error())
		return
	}

	base := p.BaseURL
	if base == "" {
		base = provider.DefaultBaseURL(provider.KindDeepSeekW)
	}
	conv := dsConvoKey(body)

	tried := map[string]bool{}
	sessKey := provider.SessionKey(p.ID, sessionFingerprint(body))
	var lastErr error
	maxAttempts := s.maxAccountAttempts()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		acct := s.pool.PickForSession(p, tried, sessKey)
		uc.accountID = acct.ID
		if acct == nil {
			if attempt == 0 {
				uc.fail(p.ID, realModel)
				s.fail(w, http.StatusBadRequest, fmt.Sprintf("供应商 %s 没有可用账号", p.ID))
				return
			}
			break
		}
		if attempt > 0 {
			s.switches.Add(1)
			log.Printf("换号重试: %s/%s（第 %d 次）", p.ID, acct.ID, attempt+1)
		}
		tried[acct.ID] = true

		client, cerr := deepseekweb.NewClient(acct.Key())
		if cerr != nil {
			uc.fail(p.ID, realModel)
			s.pool.Fail(p.ID, acct.ID, 0, cerr.Error())
			lastErr = cerr
			continue
		}
		client.BaseURL = base
		// 复用网关的 HTTP 客户端：里面装着内嵌 CA 根证书，不然 Android 上会报
		// x509: certificate signed by unknown authority。
		client.HTTP = provider.HTTPClient

		key := p.ID + "|" + acct.ID + "|" + conv
		entry, aerr := dsSessions.acquire(r.Context(), key)
		if aerr != nil {
			uc.fail(p.ID, realModel)
			s.fail(w, http.StatusRequestTimeout, "等待会话空闲被取消: "+aerr.Error())
			return
		}
		sessionID, parentID := entry.ids()

		relay := newDSRelay(w, stream, proto, alias)
		var st *deepseekweb.StreamState
		if stream {
			st = deepseekweb.NewStreamState(relay.onText, relay.onReasoning)
		} else {
			// 非流式不需要回调，Run 结束后从 StreamState 里取累积文本
			st = deepseekweb.NewStreamState(nil, nil)
		}

		res, rerr := client.Run(r.Context(), body, sessionID, parentID, st, dsPowTimeout)
		if rerr != nil {
			entry.fail()
			dsSessions.release(key, entry)
			s.pool.Fail(p.ID, acct.ID, dsStatusOf(rerr), truncate(rerr.Error(), 160))
			lastErr = rerr
			// 已经流出正文（可能还带了用量）时按实际用量记，否则只记失败
			uc.record(p.ID, realModel, false, dsUsageOf(st))
			if relay.started {
				// 已经把响应头发给客户端，改不了状态码，只能断流（客户端读到 EOF 会当结束）
				log.Printf("DeepSeek 网页版流中断: %v", rerr)
				return
			}
			if !dsRetryable(rerr) {
				s.fail(w, dsGatewayStatus(rerr), "DeepSeek 网页版上游失败: "+rerr.Error())
				return
			}
			continue
		}

		entry.commit(res.SessionID, res.Stream.MessageID, res.Stream.HasMessageID)
		dsSessions.release(key, entry)
		s.pool.Success(p.ID, acct.ID)
		uc.record(p.ID, realModel, true, dsUsageOf(res.Stream))
		if ferr := relay.finish(res.Stream); ferr != nil {
			log.Printf("DeepSeek 网页版回写失败: %v", ferr)
		}
		return
	}

	uc.fail(p.ID, realModel)
	if lastErr != nil {
		s.fail(w, http.StatusBadGateway, "DeepSeek 网页版所有账号尝试均失败: "+lastErr.Error())
		return
	}
	s.fail(w, http.StatusBadGateway, "DeepSeek 网页版所有账号尝试均失败")
}

// dsUsageOf 把 DeepSeek 网页版的 StreamState 用量转成统计口径。
//
// 上游只给 accumulated_token_usage（不区分输入输出），所以整体记进输出侧，
// 面板看到的总量是对的，拆分口径会偏。
func dsUsageOf(st *deepseekweb.StreamState) usageTokens {
	if st == nil || st.Usage <= 0 {
		return usageTokens{}
	}
	return usageTokens{Total: int(st.Usage), Out: int(st.Usage)}
}

// dsStatusOf 取上游 HTTP 状态码（没有就当 0，交给账号池按网络错误处理）。
func dsStatusOf(err error) int {
	var ue *deepseekweb.UpstreamError
	if errors.As(err, &ue) {
		return ue.UpstreamStatus
	}
	return 0
}
