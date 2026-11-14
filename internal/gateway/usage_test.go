package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// resetUsage 清空统计表，保证每个用例互不干扰。
func resetUsage(t *testing.T) {
	t.Helper()
	usageMu.Lock()
	usageTable = map[string]*usageStat{}
	usageDaily = map[string]map[string]*usageStat{}
	usageMu.Unlock()
}

// usageRow 取某条统计行的拷贝（tests 里断言用）。
func usageRow(key string) (req, errs, in, out int64, ok bool) {
	usageMu.RLock()
	defer usageMu.RUnlock()
	st, ok := usageTable[key]
	if !ok {
		return 0, 0, 0, 0, false
	}
	return st.Requests.Load(), st.Errors.Load(), st.InTokens.Load(), st.OutTokens.Load(), true
}

// OpenAI 流式：最后一帧的 usage 要被抽出来（[DONE] 和非 JSON 行不能把它冲掉）。
func TestUsageFromOpenAIStream(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22,\"total_tokens\":33}}\n\n" +
		"data: [DONE]\n\n"

	var insp sseInspector
	// 故意按任意字节边界喂，覆盖「一帧跨多次 read」
	for i := 0; i < len(stream); i += 7 {
		end := i + 7
		if end > len(stream) {
			end = len(stream)
		}
		insp.feed([]byte(stream[i:end]))
	}
	u := insp.result()
	if u.In != 11 || u.Out != 22 || u.Total != 33 {
		t.Fatalf("usage = %+v，期望 in=11 out=22 total=33", u)
	}
}

// Anthropic 流式：input 在 message_start.message.usage，output 在 message_delta.usage。
func TestUsageFromAnthropicStream(t *testing.T) {
	stream := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":2}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	var insp sseInspector
	insp.feed([]byte(stream))
	u := insp.result()
	if u.In != 7 || u.Out != 9 {
		t.Fatalf("usage = %+v，期望 in=7（5+2 缓存）out=9", u)
	}
}

// 没有 usage 的流不能报错，也不能把请求数弄丢（提取层返回 0）。
func TestUsageStreamWithoutUsage(t *testing.T) {
	var insp sseInspector
	insp.feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"))
	if u := insp.result(); !u.empty() {
		t.Fatalf("不该抽到 usage：%+v", u)
	}
}

// include_usage 只加到流式 OpenAI 请求上，客户端显式关掉时不覆盖。
func TestWithStreamUsage(t *testing.T) {
	out := string(withStreamUsage([]byte(`{"model":"m","stream":true,"messages":[]}`)))
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("不是 JSON: %v (%s)", err, out)
	}
	opts, _ := m["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Fatalf("没补上 include_usage: %s", out)
	}

	// 非流式不动
	ns := `{"model":"m","stream":false}`
	if got := string(withStreamUsage([]byte(ns))); got != ns {
		t.Fatalf("非流式被改了: %s", got)
	}

	// 客户端明确 false，尊重它
	no := `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`
	if got := string(withStreamUsage([]byte(no))); got != no {
		t.Fatalf("显式关闭时不该改: %s", got)
	}
}

// 记账：key 必须是 providerID/model，请求数与 token 都要落账，且同时进当天的桶。
func TestUsageCtxRecord(t *testing.T) {
	resetUsage(t)

	uc := newUsageCtx("fake/m1")
	uc.record("real-provider", "real-model", true, usageTokens{In: 3, Out: 4})

	req, errs, in, out, ok := usageRow("real-provider/real-model")
	if !ok {
		t.Fatal("没有记到 real-provider/real-model")
	}
	if req != 1 || errs != 0 || in != 3 || out != 4 {
		t.Fatalf("计数 = req%d err%d in%d out%d", req, errs, in, out)
	}

	// 同一个 ctx 只记一次
	uc.record("other", "other", true, usageTokens{In: 9})
	if _, _, _, _, ok := usageRow("other/other"); ok {
		t.Fatal("同一 ctx 被重复记账")
	}

	// 按天桶里也要有
	usageMu.RLock()
	dayTable := usageDaily[time.Now().Format("2006-01-02")]
	usageMu.RUnlock()
	if dayTable == nil {
		t.Fatal("没有按天聚合")
	}
	st := dayTable["real-provider/real-model"]
	if st == nil || st.Requests.Load() != 1 || st.InTokens.Load() != 3 {
		t.Fatalf("按天计数不对: %+v", st)
	}
}

// 没有 token 的失败也要落请求数与错误数（请求数不能丢）。
func TestUsageCtxFailWithoutTokens(t *testing.T) {
	resetUsage(t)
	uc := newUsageCtx("p/m")
	uc.fail()
	req, errs, _, _, ok := usageRow("p/m")
	if !ok || req != 1 || errs != 1 {
		t.Fatalf("失败请求没记上: ok=%v req=%d errs=%d", ok, req, errs)
	}
}

// /api/usage/summary 要给出总量、成功率与按供应商分组。
func TestUsageSummaryShape(t *testing.T) {
	resetUsage(t)
	recordUsage("provA", "m1", true, 10, 20)
	recordUsage("provA", "m1", false, 0, 0)
	recordUsage("provB", "m2", true, 5, 5)

	rec := httptest.NewRecorder()
	s := &Server{}
	s.handleUsageSummary(rec, httptest.NewRequest(http.MethodGet, "/api/usage/summary", nil))

	var got struct {
		Requests    int64   `json:"requests"`
		Errors      int64   `json:"errors"`
		OkRequests  int64   `json:"okRequests"`
		TotalTokens int64   `json:"totalTokens"`
		SuccessRate float64 `json:"successRate"`
		Providers   []struct {
			Provider string `json:"provider"`
			Requests int64  `json:"requests"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("summary 不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if got.Requests != 3 || got.Errors != 1 || got.OkRequests != 2 {
		t.Errorf("总量 = %+v", got)
	}
	if got.TotalTokens != 40 {
		t.Errorf("totalTokens = %d，期望 40", got.TotalTokens)
	}
	if got.SuccessRate < 0.66 || got.SuccessRate > 0.67 {
		t.Errorf("成功率 = %v，期望约 0.667", got.SuccessRate)
	}
	if len(got.Providers) != 2 || got.Providers[0].Provider != "provA" || got.Providers[0].Requests != 2 {
		t.Errorf("按供应商分组 = %+v", got.Providers)
	}
}
