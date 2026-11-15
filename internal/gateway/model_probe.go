package gateway

import (
	"context"
	"encoding/json"
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
// 背景：拉到的模型列表不等于「能用」。上游会按账号类型限制：
//
//	code 11158  model not allowed for user type [personal_free_user]
//	code 11102  service info not found
//
// 用户看到一堆模型实际大半调不通。这里提供一个主动探测接口：
// 对每个模型发一个极小的请求（max_tokens=1），把结果标成可用/受限/不存在。

// ModelProbe 是单个模型的探测结果。
type ModelProbe struct {
	Model  string `json:"model"`
	OK     bool   `json:"ok"`
	Status string `json:"status"` // ok / not_allowed / not_found / error
	Code   int    `json:"code,omitempty"`
	Msg    string `json:"msg,omitempty"`
	MS     int64  `json:"ms"`
}

// handleModelProbe 探测一批模型是否可用。
//
//	POST /api/providers/models/probe
//	{"providerId":"workbuddy","accountId":"可选","models":["a","b"]}   不传 models 就探全部
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
	if workbuddyProviderKind(p) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "目前只支持 WorkBuddy 系列的可用性探测"})
		return
	}

	// 选账号：优先指定的，否则第一个可用的
	var acc *config.Account
	for _, a := range p.Accounts {
		if a == nil || !a.IsEnabled() || a.Key() == "" || a.UID == "" {
			continue
		}
		if in.AccountID != "" && a.ID != in.AccountID {
			continue
		}
		acc = a
		break
	}
	if acc == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有可用账号"})
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
	// 上限保护：一次最多探 80 个，避免把上游打爆
	if len(models) > 80 {
		models = models[:80]
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	cust := workbuddy.NewCustomer(acc.UID, acc.Key(), acc.Domain, "", "", p.ID == "workbuddy-ai")

	type res struct {
		idx   int
		probe ModelProbe
	}
	ch := make(chan res, len(models))
	// 并发 4 个，兼顾速度与不触发风控
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, m := range models {
		wg.Add(1)
		go func(idx int, model string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ch <- res{idx: idx, probe: probeOneModel(ctx, cust, model)}
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
		// 可用的排前面
		if out[i].OK != out[j].OK {
			return out[i].OK
		}
		return out[i].Model < out[j].Model
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"account": acc.Name,
		"total":   len(out),
		"usable":  okCount,
		"unusable": badCount,
		"results": out,
		"hint":    "可用 = 上游接受并返回了内容；受限 = 账号类型不允许；不存在 = 该站没有这个模型",
	})
}

// probeOneModel 发一个 max_tokens=1 的最小请求判断模型能不能用。
func probeOneModel(ctx context.Context, c workbuddy.Customer, model string) ModelProbe {
	start := time.Now()
	mp := ModelProbe{Model: model}
	body := map[string]any{
		"model":      model,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     true,
	}
	raw, err := c.Chat(ctx, body)
	mp.MS = time.Since(start).Milliseconds()
	if err != nil {
		mp.Status = "error"
		mp.Msg = truncateStr(err.Error(), 120)
		return mp
	}
	// Chat 返回的是原始响应信封，解析 code/msg
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
		case 6004:
			mp.Status = "rate_limited"
		default:
			mp.Status = "error"
		}
		return mp
	}
	mp.OK = true
	mp.Status = "ok"
	return mp
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// workbuddyProviderKind 判断是否 WorkBuddy 系列，返回规范化的 kind。
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
