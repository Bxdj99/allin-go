// Package gateway 是 HTTP 入口：对外提供 OpenAI / Anthropic 兼容接口，
// 对内按 model 名路由到上游供应商，并在协议不一致时自动互转。
package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"allin/internal/bridge"
	"allin/internal/config"
	"allin/internal/prompt"
	"allin/internal/provider"
	"allin/internal/provider/codex"
	"allin/internal/provider/qoder"
	"allin/internal/provider/trae"
	"allin/internal/provider/workbuddy"
)

// maxBodyBytes 是单个请求体的上限，防止有人拿它当上传通道。
const maxBodyBytes = 8 << 20 // 8 MiB

// maxAccountAttempts 是一条请求最多尝试几个账号（换号重试）。
const maxAccountAttempts = 3

// Server 是网关实例。
type Server struct {
	store *config.Store
	pool  *provider.Pool
	mux   *http.ServeMux

	requests atomic.Int64
	errors   atomic.Int64
	switches atomic.Int64
	started  time.Time
}

// New 构造网关。
func New(store *config.Store) *Server {
	s := &Server{store: store, pool: provider.NewPool(), mux: http.NewServeMux(), started: time.Now()}
	s.routes()
	return s
}

func (s *Server) routes() {
	// 模型接口
	s.mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	s.mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChat))
	s.mux.HandleFunc("/v1/messages", s.withAuth(s.handleMessages))
	s.mux.HandleFunc("/v1/responses", s.withAuth(s.handleResponses))
	s.mux.HandleFunc("/v1/images/generations", s.withAuth(s.handleImages))

	// 管理接口
	s.mux.HandleFunc("/api/status", s.withAuth(s.handleStatus))
	s.mux.HandleFunc("/api/config", s.withAuth(s.handleConfig))
	s.mux.HandleFunc("/api/models", s.withAuth(s.handleModels))
	s.mux.HandleFunc("/api/accounts", s.withAuth(s.handleAccounts))
	s.mux.HandleFunc("/api/providers", s.withAuth(s.handleProvidersOrWrite))
	s.mux.HandleFunc("/api/providers/write", s.withAuth(s.handleProviderWrite))
	s.mux.HandleFunc("/api/accounts/write", s.withAuth(s.handleAccountWrite))
	s.mux.HandleFunc("/api/prompt", s.withAuth(s.handlePrompt))
	s.mux.HandleFunc("/api/prompt/preset", s.withAuth(s.handlePromptPreset))
	s.mux.HandleFunc("/api/keys", s.withAuth(s.handleKeys))
	s.mux.HandleFunc("/api/keys/toggle", s.withAuth(s.handleKeyToggle))
	s.mux.HandleFunc("/api/usage", s.withAuth(s.handleUsage))
	s.mux.HandleFunc("/api/usage/reset", s.withAuth(s.handleUsageReset))
	s.mux.HandleFunc("/api/auth/workbuddy/start", s.withAuth(s.handleWorkBuddyStart))
	s.mux.HandleFunc("/api/auth/workbuddy/poll", s.withAuth(s.handleWorkBuddyPoll))
	s.mux.HandleFunc("/api/auth/github-copilot/start", s.withAuth(s.handleCopilotStart))
	s.mux.HandleFunc("/api/auth/github-copilot/poll", s.withAuth(s.handleCopilotPoll))
	s.mux.HandleFunc("/api/auth/trae/start", s.withAuth(s.handleTraeStart))
	s.mux.HandleFunc("/api/auth/trae/poll", s.withAuth(s.handleTraePoll))
	// 第二批供应商登录：antigravity catpaw cline codex minimax qoder raccoon wenxin
	// 统一走 /api/auth/<vendor>/start 与 /api/auth/<vendor>/poll（见 provider_auth.go）。
	// 上面三条精确路径优先匹配，这里只兜住其余子路径。
	s.mux.HandleFunc("/api/auth/", s.withAuth(s.handleProviderAuth))
	s.mux.HandleFunc("/api/workbuddy/checkin", s.withAuth(s.handleWorkBuddyCheckin))
	s.mux.HandleFunc("/api/workbuddy/checkin-status", s.withAuth(s.handleWorkBuddyCheckin))
	// 任务中心：POST 跑全套（report→checkin→streak→claim-gift→compensation→
	// makeup→buddy→travel→lottery），GET 只读汇总。
	s.mux.HandleFunc("/api/workbuddy/tasks", s.withAuth(s.handleWorkBuddyTasks))
	s.mux.HandleFunc("/api/workbuddy/tasks/status", s.withAuth(s.handleWorkBuddyTaskStatus))

	// 面板
	s.mux.HandleFunc("/", s.handleIndex)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// ---------- 鉴权 ----------

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.store.Snapshot()
		if cfg.Server.AuthKey == "" && len(cfg.APIKeys) == 0 {
			next(w, r)
			return
		}
		got := bearerOf(r)
		if got == "" {
			got = r.URL.Query().Get("key")
		}
		if !keyAllowed(cfg, got) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "需要网关口令",
				"hint":  "请求头带 Authorization: Bearer <authKey>，或在 URL 加 ?key=<authKey>",
			})
			return
		}
		next(w, r)
	}
}

func bearerOf(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

// ---------- 模型列表 ----------

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	names := provider.ModelList(cfg)
	data := make([]map[string]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]any{
			"id":       n,
			"object":   "model",
			"created":  s.started.Unix(),
			"owned_by": "allin",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ---------- 转发核心 ----------

type protocol string

const (
	protocolOpenAI    protocol = "openai"
	protocolAnthropic protocol = "anthropic"
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/chat/completions", protocolOpenAI)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/messages", protocolAnthropic)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/responses", protocolOpenAI)
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/images/generations", protocolOpenAI)
}

// forward 是核心：改写 model → 选账号 → 必要时转换协议 → 发上游 → 失败换号重试。
func (s *Server) forward(w http.ResponseWriter, r *http.Request, apiPath string, proto protocol) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	s.requests.Add(1)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}

	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		s.fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}

	cfg := s.store.Snapshot()
	p, _, realModel, err := provider.Resolve(cfg, head.Model)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	kind := p.Kind
	if kind == "" {
		kind = provider.KindOpenAI
	}
	kind = provider.NormalizeKind(kind)
	// 网页版私有协议三家（妙手 / 文心 / Antigravity）整条链路自己走：请求拼装、会话机、
	// 信封、HTTP/1.1 约束都在各自包里，套不进下面「BuildRequest + 透传/桥接」的框架。
	switch kind {
	case provider.KindCatPaw, provider.KindWenxin, provider.KindAntigravity:
		s.forwardWebChat(w, r, p, kind, realModel, body, head.Stream, proto)
		return
	}
	// DeepSeek 网页版整条链路自己走：端点、PoW、会话链、patch 流都和 OpenAI 兼容
	// 上游不一样，套不进下面的「透传 / 协议桥接」框架。
	if kind == provider.KindDeepSeekW {
		s.forwardDeepSeek(w, r, p, realModel, body, head.Stream, proto)
		return
	}
	// 上游协议：workbuddy 这类特殊供应商走 OpenAI 形态
	upstreamProto := protocolOpenAI
	if kind == provider.KindAnthropic || kind == provider.KindMiniMax {
		upstreamProto = protocolAnthropic
	}

	// 客户端协议与上游协议不一致时做转换
	needBridge := proto != upstreamProto
	upstreamBody := body
	// 跨协议时路径必须跟着上游协议走，否则会打到不存在的端点
	// （Anthropic 客户端 /v1/messages → OpenAI 上游，仍拼 /messages 就是 404）
	if needBridge {
		if upstreamProto == protocolAnthropic {
			apiPath = "/messages"
		} else {
			apiPath = "/chat/completions"
		}
	}
	if needBridge {
		if head.Stream {
			s.fail(w, http.StatusBadRequest,
				"当前版本还不支持跨协议的流式转换（客户端是 "+string(proto)+"，上游是 "+string(upstreamProto)+"），请把 stream 设为 false")
			return
		}
		if proto == protocolAnthropic && upstreamProto == protocolOpenAI {
			upstreamBody, err = bridge.AnthropicToOpenAI(body, realModel)
		} else if proto == protocolOpenAI && upstreamProto == protocolAnthropic {
			upstreamBody, err = bridge.OpenAIToAnthropicRequest(body, realModel)
		}
		if err != nil {
			s.fail(w, http.StatusBadRequest, "协议转换失败: "+err.Error())
			return
		}
	} else {
		upstreamBody, err = rewriteModel(body, realModel)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "改写请求体失败: "+err.Error())
			return
		}
	}

	// WorkBuddy 需要请求体适配：工具名规范化、tool_choice 归一化、补 reasoning_effort
	if kind == provider.KindWorkBuddy {
		effort := ""
		if p.Headers != nil {
			effort = p.Headers["X-Allin-Effort"]
		}
		if prepared, perr := workbuddy.Prepare(upstreamBody, effort); perr == nil {
			upstreamBody = prepared
		} else {
			log.Printf("WorkBuddy 请求预处理失败（按原样发送）: %v", perr)
		}
		// WorkBuddy 只接受流式：上游一律 stream:true，客户端要非流式时本地聚合
		if forced, ferr := workbuddy.ForceStream(upstreamBody); ferr == nil {
			upstreamBody = forced
		}
	}

	// Codex 走 Responses 协议，请求体要从 chat 形态转过去
	if kind == provider.KindCodex {
		if converted, cerr := codex.BuildChatRequest(upstreamBody, realModel); cerr == nil {
			if fin, ferr := codex.FinalizeResponses(converted); ferr == nil {
				upstreamBody = fin
			} else {
				upstreamBody = converted
			}
		} else {
			log.Printf("Codex 请求转换失败: %v", cerr)
		}
	}

	// 系统提示词注入：网关层强制，客户端改不掉
	if ptext, perr := loadPromptText(cfg); perr == nil && ptext != "" {
		if upstreamProto == protocolAnthropic {
			if nb, aerr := prompt.ApplyAnthropic(upstreamBody, ptext, cfg.Prompt.Mode); aerr == nil {
				upstreamBody = nb
			}
		} else {
			if nb, aerr := prompt.ApplyOpenAI(upstreamBody, ptext, cfg.Prompt.Mode); aerr == nil {
				upstreamBody = nb
			}
		}
	} else if perr != nil {
		log.Printf("提示词注入失败（按原样发送）: %v", perr)
	}

	base := p.BaseURL
	if base == "" {
		base = provider.DefaultBaseURL(kind)
	}
	if base == "" {
		s.fail(w, http.StatusBadRequest, fmt.Sprintf("供应商 %s 没有配置 baseUrl", p.ID))
		return
	}

	// Trae 的响应是它自己的事件流，必须逐帧转换，不能透传；转换时要认识「声明过哪些工具」，
	// 否则正文里文本形式的工具调用会被当成普通文字。工具表从送给上游的那份请求体里取。
	var traeTools []trae.ToolSpec
	if kind == provider.KindTrae {
		traeTools = trae.ToolSpecs(upstreamBody)
	}

	// 账号轮转：失败换号重试
	tried := map[string]bool{}
	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		acct := s.pool.Pick(p, tried)
		if acct == nil {
			if attempt == 0 {
				s.fail(w, http.StatusBadRequest, fmt.Sprintf("供应商 %s 没有可用账号", p.ID))
			} else {
				s.fail(w, http.StatusBadGateway, fmt.Sprintf("供应商 %s 的账号都试过了，全部失败", p.ID))
			}
			return
		}
		if attempt > 0 {
			s.switches.Add(1)
			log.Printf("换号重试: %s/%s（第 %d 次）", p.ID, acct.ID, attempt+1)
		}
		tried[acct.ID] = true

		upreq, err := provider.BuildRequest(kind, base, apiPath, acct.Key(), acct, upstreamBody, p.Headers)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "构造上游请求失败: "+err.Error())
			return
		}
		if (head.Stream || kind == provider.KindWorkBuddy || kind == provider.KindTrae) && !needBridge {
			upreq.Header.Set("Accept", "text/event-stream")
		}
		upreq = upreq.WithContext(r.Context())

		resp, err := provider.HTTPClient.Do(upreq)
		if err != nil {
			s.pool.Fail(p.ID, acct.ID, 0, err.Error())
			log.Printf("上游请求失败 %s/%s: %v", p.ID, acct.ID, err)
			continue
		}

		// 需要换号的状态码
		if resp.StatusCode == 401 || resp.StatusCode == 429 || resp.StatusCode >= 500 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			s.pool.Fail(p.ID, acct.ID, resp.StatusCode, truncate(string(raw), 160))
			log.Printf("上游 %d，换号: %s/%s", resp.StatusCode, p.ID, acct.ID)
			continue
		}

		s.pool.Success(p.ID, acct.ID)
		// Qoder 的上游永远是 SSE，而且每帧 data 还套了一层 {statusCodeValue, body} 信封，
		// 不能透传字节流：流式要逐帧解成标准 OpenAI 事件，非流式在本地聚合成一个
		// chat.completion（必要时再转成 Anthropic 形态）。
		if kind == provider.KindQoder {
			s.relayQoder(w, resp, head.Stream, needBridge, proto, realModel)
			return
		}
		// Trae 的上游响应是自定义 SSE 事件流，必须转换后才能给客户端；
		// 客户端要非流式时在本地把整条流聚合成一个 chat.completion。
		if kind == provider.KindTrae {
			s.relayTrae(w, resp, head.Stream, needBridge, proto, realModel, traeTools)
			return
		}
		// WorkBuddy 上游一定是流式。客户端要非流式时先聚合，
		// 需要跨协议时再拿聚合结果去转换（这里容易踩：先转协议会把 SSE 当 JSON 解析）
		if kind == provider.KindWorkBuddy && !head.Stream {
			aggregated, aerr := workbuddy.Aggregate(resp.Body, realModel)
			resp.Body.Close()
			if aerr != nil {
				s.fail(w, http.StatusBadGateway, "WorkBuddy 响应聚合失败: "+aerr.Error())
				return
			}
			if needBridge && proto == protocolAnthropic {
				converted, cerr := bridge.OpenAIToAnthropic(aggregated, realModel)
				if cerr != nil {
					s.fail(w, http.StatusBadGateway, "响应协议转换失败: "+cerr.Error())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(converted)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(aggregated)
			return
		}
		s.relay(w, resp, head.Stream, needBridge, proto, realModel)
		return
	}

	s.fail(w, http.StatusBadGateway, "所有账号尝试均失败")
}

// relay 把上游响应回写给客户端，必要时转换协议。
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, stream, needBridge bool, clientProto protocol, model string) {
	defer resp.Body.Close()

	// 流式且不需要转换：直接透传字节流（最快路径）
	if stream && !needBridge {
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "text/event-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		flushCopy(w, resp.Body)
		return
	}

	// 非流式：完整读取后按需转换
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		s.fail(w, http.StatusBadGateway, "读取上游响应失败: "+err.Error())
		return
	}
	if !needBridge {
		// 顺手抽 usage 记进统计表（拿不到就记 0）
		inTok, outTok := extractUsage(raw)
		recordUsage(resp.Request.URL.Host, model, resp.StatusCode < 400, inTok, outTok)
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	var converted []byte
	if clientProto == protocolAnthropic {
		converted, err = bridge.OpenAIToAnthropic(raw, model)
	} else {
		converted, err = bridge.AnthropicToOpenAIResponse(raw, model)
	}
	if err != nil {
		s.fail(w, http.StatusBadGateway, "响应协议转换失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(converted)
}

// relayTrae 把 Trae 的上游事件流转成标准 OpenAI 响应。
//
// Trae 上游只会回它自己的 SSE（event/data 帧），必须逐帧转换后写给客户端，
// 不能像其他供应商那样直接透传字节流；客户端要非流式时用 ConvertStream 整条聚合。
func (s *Server) relayTrae(w http.ResponseWriter, resp *http.Response, stream, needBridge bool, clientProto protocol, model string, tools []trae.ToolSpec) {
	defer resp.Body.Close()

	// 上游明确报错时回的是普通 JSON（不是事件流），原样转出去让客户端看清原因。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	// 非流式：把整条上游流聚合成一个 chat.completion。
	if !stream {
		out, err := trae.ConvertStreamWithTools(resp.Body, model, tools)
		if err != nil {
			log.Printf("Trae 响应聚合失败: %v", err)
			s.fail(w, http.StatusBadGateway, "Trae 响应聚合失败: "+err.Error())
			return
		}
		if needBridge {
			converted, cerr := bridge.OpenAIToAnthropic(out, model)
			if cerr != nil {
				s.fail(w, http.StatusBadGateway, "响应协议转换失败: "+cerr.Error())
				return
			}
			out = converted
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}

	// 流式：一行一行喂给状态机，转换结果逐帧写回。
	st := trae.NewStreamState(model)
	if err := st.DeclareTools(tools); err != nil {
		s.fail(w, http.StatusBadGateway, "Trae 工具声明失败: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	send := func(frames []string) bool {
		for _, f := range frames {
			if _, err := io.WriteString(w, f); err != nil {
				return false
			}
		}
		if canFlush && len(frames) > 0 {
			flusher.Flush()
		}
		return true
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20) // 单帧上限 1 MiB，留够余量
	for sc.Scan() {
		frames, err := trae.TransformEvent(sc.Text(), st)
		if err != nil {
			// 已经开始写响应体，改不了状态码，只能收尾（客户端读到 EOF 会视为结束）。
			log.Printf("Trae 流转换中断: %v", err)
			return
		}
		if !send(frames) {
			return
		}
		if st.Terminal {
			return
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("Trae 上游读取中断: %v", err)
		return
	}
	// 兜底：最后一个没以空行收尾的帧补发掉（含末尾的 [DONE]）。
	frames, err := trae.Flush(st)
	if err != nil {
		log.Printf("Trae 流收尾失败: %v", err)
		return
	}
	send(frames)
}

// relayQoder 把 Qoder 的上游事件流转成标准 OpenAI 响应。
//
// Qoder 的聊天端点只给 SSE，而且每帧 data 外面还包着 {"statusCodeValue":200,"body":"…"}，
// body 里才是 OpenAI 形态的 chunk（请求带 Encode=1 时 body 还可能被自定义 base64 编过）。
// 所以这里不能透传字节流：流式逐帧解出来重写，非流式整条聚合成一个 chat.completion。
func (s *Server) relayQoder(w http.ResponseWriter, resp *http.Response, stream, needBridge bool, clientProto protocol, model string) {
	defer resp.Body.Close()

	// 上游明确报错时回的是普通 JSON（不是事件流），原样转出去让客户端看清原因。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	// 非流式：整条流聚合成一个 chat.completion，需要时再转成 Anthropic 形态。
	if !stream {
		out, err := qoder.Aggregate(resp.Body, model)
		if err != nil {
			log.Printf("Qoder 响应聚合失败: %v", err)
			s.fail(w, http.StatusBadGateway, "Qoder 响应聚合失败: "+err.Error())
			return
		}
		if needBridge && clientProto == protocolAnthropic {
			converted, cerr := bridge.OpenAIToAnthropic(out, model)
			if cerr != nil {
				s.fail(w, http.StatusBadGateway, "响应协议转换失败: "+cerr.Error())
				return
			}
			out = converted
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	// 解一帧写一帧并 flush，客户端才能看到增量。
	dst := &flushWriter{w: w, canFlush: canFlush}
	if err := qoder.RelayStream(dst, resp.Body); err != nil {
		// 已经开始写响应体，改不了状态码，只能收尾（客户端读到 EOF 会视为结束）。
		log.Printf("Qoder 流转换中断: %v", err)
		return
	}
	if canFlush {
		flusher.Flush()
	}
}

// flushWriter 让 qoder 的逐帧写出立即落到客户端。
type flushWriter struct {
	w        http.ResponseWriter
	canFlush bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.canFlush {
		if flusher, ok := f.w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return n, err
}

func flushCopy(w http.ResponseWriter, body io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			log.Printf("上游读取中断: %v", rerr)
			return
		}
	}
}

// rewriteModel 只替换 JSON 里的 model 字段，保持其余字节不变。
func rewriteModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = raw
	return json.Marshal(m)
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string) {
	s.errors.Add(1)
	writeJSON(w, code, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "gateway_error",
		},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// decodeJSON 读取有上限的 JSON 请求体。
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// extractUsage 从响应体里抽 prompt_tokens / completion_tokens。
func extractUsage(raw []byte) (int, int) {
	var env struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return 0, 0
	}
	in := env.Usage.PromptTokens
	if in == 0 {
		in = env.Usage.InputTokens
	}
	out := env.Usage.CompletionTokens
	if out == 0 {
		out = env.Usage.OutputTokens
	}
	return in, out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
