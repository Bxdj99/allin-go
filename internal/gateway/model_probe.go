package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"allin/internal/config"
	"allin/internal/provider/workbuddy"
)

// 模型可用性探测。
//
// 背景：拉到的模型列表不等于「能用」。上游会按账号类型/额度限制：
//
//	11158  model not allowed for user type
//	11102  service info not found
//	6004   使用量已超出频率限制
//
// 这里提供一个主动探测接口：真的发一次请求，把结果标成可用/受限。
//
// 支持范围：WorkBuddy 走专用探测（能识别业务错误码），其它供应商走
// 通用网关自调用探测（走完整的 forward 路径，能反映真实可用性）。

// ModelProbe 是单个模型的探测结果。
type ModelProbe struct {
	Model  string `json:"model"`
	OK     bool   `json:"ok"`
	Status string `json:"status"` // ok / not_allowed / not_found / rate_limited / empty / error
	Code   int    `json:"code,omitempty"`
	Msg    string `json:"msg,omitempty"`
	MS     int64  `json:"ms"`
}

// handleModelProbe 探测一批模型是否可用。
//
//	POST /api/providers/models/probe
//	{"providerId":"workbuddy","accountId":"可选","models":["a","b"]}
func (s *Server) handleModelProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var in struct {
		ProviderID string   `json:"providerId"`
		AccountID  string   `json:"accountId"`
		Models     []string `json:"models"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败"})
		return
	}
	cfg := s.store.Snapshot()
	p := cfg.FindProvider(strings.TrimSpace(in.ProviderID))
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "供应商不存在"})
		return
	}

	models := in.Models
	if len(models) == 0 {
		models = p.Exposed()
	}
	if len(models) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": []any{}, "note": "没有要探测的模型"})
		return
	}
	if len(models) > 80 {
		models = models[:80]
	}

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()

	// WorkBuddy 走专用探测（能拿业务码）；其它家走通用探测
	wbKind := workbuddyProviderKind(p) != ""
	var probeFn func(model string) ModelProbe

	if wbKind {
		// 为什么收集【一批】候选账号而不是挑一个：
		// 实测 14 个账号里有 4 个 access token 已失效（HTTP 401）。
		// 旧实现只取第一个就 break，正好挑中失效账号时，
		// 整页 17 个模型会全部标成「错误」——但换任何好账号都是 ok。
		// 用户看到的就是「我这模型都失败咋回事」，实际只是探测选错了号。
		var cands []*config.Account
		for _, a := range p.Accounts {
			if a == nil || !a.IsEnabled() || a.Key() == "" || a.UID == "" {
				continue
			}
			if in.AccountID != "" && a.ID != in.AccountID {
				continue
			}
			cands = append(cands, a)
		}
		if len(cands) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有可用账号"})
			return
		}
		// 用「先挑一个健康的当基准」的方式构造探测函数
		probeFn = func(model string) ModelProbe {
			return probeWorkBuddyWithFallback(ctx, p.ID, cands, model)
		}
	} else {
		probeFn = func(model string) ModelProbe { return s.probeViaGateway(ctx, p.ID, model) }
	}

	type res struct {
		idx   int
		probe ModelProbe
	}
	ch := make(chan res, len(models))
	sem := make(chan struct{}, 4) // 并发 4，兼顾速度与不触发风控
	var wg sync.WaitGroup
	for i, m := range models {
		wg.Add(1)
		go func(idx int, model string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ch <- res{idx: idx, probe: probeFn(model)}
		}(i, m)
	}
	go func() { wg.Wait(); close(ch) }()

	out := make([]ModelProbe, len(models))
	for r := range ch {
		out[r.idx] = r.probe
	}

	okCount, badCount := 0, 0
	for _, mp := range out {
		if mp.OK {
			okCount++
		} else {
			badCount++
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OK != out[j].OK {
			return out[i].OK
		}
		return out[i].Model < out[j].Model
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"provider":  p.ID,
		"total":     len(out),
		"usable":    okCount,
		"unusable":  badCount,
		"results":   out,
		"supported": wbKind,
		"hint": "可用 = 上游真的产出了内容；受限 = 账号类型不允许；" +
			"不存在 = 该站没有这个模型；额度尽 = 被限流；空响应 = 上游接受但没产出",
	})
}

// probeWorkBuddyModel 用最小请求判断 WorkBuddy 模型能不能用。
//
// 注意 max_tokens 不能给太小：推理类模型 1 个 token 连首帧都不产生，
// 上游也不做完整校验，会误判成「可用」。
func probeWorkBuddyModel(ctx context.Context, c workbuddy.Customer, model string) ModelProbe {
	start := time.Now()
	mp := ModelProbe{Model: model}
	body := map[string]any{
		"model":      model,
		"messages":   []map[string]any{{"role": "user", "content": "1+1=?"}},
		"max_tokens": 64,
		"stream":     true,
	}
	raw, err := c.Chat(ctx, body)
	mp.MS = time.Since(start).Milliseconds()
	if err != nil {
		mp.Status = "error"
		mp.Msg = truncateStr(err.Error(), 120)
		return mp
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
		mp.Code = env.Code
		mp.Msg = truncateStr(env.Msg, 120)
		switch env.Code {
		case 11158:
			mp.Status = "not_allowed"
		case 11102:
			mp.Status = "not_found"
		case 6004, 4011:
			mp.Status = "rate_limited"
		default:
			mp.Status = "error"
		}
		return mp
	}
	if !streamHasContent(raw) {
		mp.Status = "empty"
		mp.Msg = "上游接受了请求但没有产出内容（可能额度不足或模型不可用）"
		return mp
	}
	mp.OK = true
	mp.Status = "ok"
	return mp
}

// probeViaGateway 用通用路径探测：直接打自己的 /v1/chat/completions。
//
// 好处是走完整链路（含认证、协议转换、各家 relay），坏处是拿不到业务码，
// 只能根据 HTTP 状态与错误文本归类。
func (s *Server) probeViaGateway(ctx context.Context, providerID, model string) ModelProbe {
	start := time.Now()
	mp := ModelProbe{Model: model}

	port := s.listenPort()
	if port <= 0 {
		mp.Status = "error"
		mp.Msg = "网关端口未知"
		return mp
	}
	// Go 版没有独立管理凭据，用配置里的 authKey
	key := s.store.Snapshot().Server.AuthKey
	if key == "" {
		mp.Status = "error"
		mp.Msg = "网关没有设置口令"
		return mp
	}

	payload, _ := json.Marshal(map[string]any{
		"model":      providerID + "/" + model,
		"messages":   []map[string]any{{"role": "user", "content": "1+1=?"}},
		"max_tokens": 64,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://127.0.0.1:"+itoaPort(port)+"/v1/chat/completions", strings.NewReader(string(payload)))
	if err != nil {
		mp.Status = "error"
		mp.Msg = err.Error()
		return mp
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := probeHTTPClient.Do(req)
	mp.MS = time.Since(start).Milliseconds()
	if err != nil {
		mp.Status = "error"
		mp.Msg = truncateStr(err.Error(), 120)
		return mp
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body, 8192)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 成功也可能是空内容
		var env struct {
			Choices []struct {
				Message struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					ToolCalls []any  `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &env) == nil && len(env.Choices) > 0 {
			m := env.Choices[0].Message
			if m.Content != "" || m.Reasoning != "" || len(m.ToolCalls) > 0 {
				mp.OK = true
				mp.Status = "ok"
				return mp
			}
		}
		mp.Status = "empty"
		mp.Msg = "上游接受了请求但没有产出内容"
		return mp
	}

	text := string(raw)
	mp.Msg = truncateStr(text, 150)
	switch {
	case resp.StatusCode == 429 || strings.Contains(text, "429") ||
		strings.Contains(text, "频率") || strings.Contains(text, "额度"):
		mp.Status = "rate_limited"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		mp.Status = "auth_failed"
	case strings.Contains(text, "not allowed") || strings.Contains(text, "not_allowed"):
		mp.Status = "not_allowed"
	case strings.Contains(text, "not found") || strings.Contains(text, "not_found"):
		mp.Status = "not_found"
	default:
		mp.Status = "error"
	}
	return mp
}

// streamHasContent 判断 SSE 流里是否有真实的正文/推理增量。
func streamHasContent(raw []byte) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					ToolCalls []any  `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Code int `json:"code"`
		}
		if json.Unmarshal([]byte(payload), &ch) != nil {
			continue
		}
		if ch.Code != 0 {
			return false
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != "" || c.Delta.Reasoning != "" || len(c.Delta.ToolCalls) > 0 {
				return true
			}
		}
	}
	return false
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// workbuddyProviderKind 判断是否 WorkBuddy 系列。
func workbuddyProviderKind(p *config.Provider) string {
	if p == nil {
		return ""
	}
	k := strings.ToLower(strings.TrimSpace(p.Kind))
	if k == "workbuddy" {
		return k
	}
	if k == "" && (p.ID == "workbuddy" || p.ID == "workbuddy-ai") {
		return "workbuddy"
	}
	return ""
}

// probeHTTPClient 是探测用的 HTTP 客户端（本地回环，不设总超时靠 ctx 控制）。
var probeHTTPClient = &http.Client{Timeout: 90 * time.Second}

// listenPort 返回当前实际监听端口。
func (s *Server) listenPort() int {
	cfg := s.store.Snapshot()
	return cfg.Server.Port
}

// readLimited 读最多 n 字节。
func readLimited(r io.Reader, n int) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, int64(n)))
}

// isCredError 判断探测结果是不是「这个账号本身的凭据坏了」。
//
// 只有这类错误才值得换账号重试。模型不允许（11158）、本站没有（11102）、
// 额度耗尽（6004/4011）都是账号/模型维度的真实结论，换号也是同一个答案，
// 重试只会白白多打几次上游。
func isCredError(mp ModelProbe) bool {
	if mp.OK {
		return false
	}
	if mp.Code == 11158 || mp.Code == 11102 || mp.Code == 6004 || mp.Code == 4011 {
		return false
	}
	m := strings.ToLower(mp.Msg)
	return strings.Contains(m, "401") ||
		strings.Contains(m, "403") ||
		strings.Contains(m, "凭据失效") ||
		strings.Contains(m, "unauthorized") ||
		strings.Contains(m, "invalid token") ||
		strings.Contains(m, "token expired")
}

// probeWorkBuddyWithFallback 在多个账号之间挑一个「能用」的来探测模型。
//
// 对同一个模型，依次用候选账号试：
//   - 拿到明确业务码（不允许 / 本站无 / 限流）→ 立刻返回，这是真实结论
//   - 凭据失效（401/403）→ 换下一个账号再试
//   - ok → 立刻返回
//
// 全部账号都凭据失效时，如实返回最后一个错误（并注明试了几个号），
// 而不是谎报成「模型不可用」。
func probeWorkBuddyWithFallback(ctx context.Context, providerID string, cands []*config.Account, model string) ModelProbe {
	if len(cands) == 0 {
		return ModelProbe{Model: model, Status: "error", Msg: "没有可用账号"}
	}
	// 最多试 5 个号：够覆盖零星的坏 token，又不至于把探测拖成几分钟
	tryN := len(cands)
	if tryN > 5 {
		tryN = 5
	}

	var last ModelProbe
	for i := 0; i < tryN; i++ {
		a := cands[i]
		cust := workbuddy.NewCustomer(a.UID, a.Key(), a.Domain, "", "", providerID == "workbuddy-ai")
		mp := probeWorkBuddyModel(ctx, cust, model)
		mp.Model = model

		if mp.OK || !isCredError(mp) {
			// 真实结论（可用 / 不允许 / 本站无 / 限流 / 空响应），直接采信
			return mp
		}
		last = mp
		// 这个号凭据坏了，换下一个
	}
	last.Msg = truncateStr("试了 "+itoaSmall(tryN)+" 个账号都是凭据失效，请重新登录："+last.Msg, 140)
	return last
}
