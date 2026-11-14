package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// traeSSE 是一段典型的 Trae 上游事件流：辅助事件 + 正文增量 + 用量 + 终止。
const traeSSE = "event: progress_notice\ndata: \"正在思考\"\n\n" +
	"event: output\ndata: {\"response\":\"你好\"}\n\n" +
	"event: output\ndata: {\"response\":\"，世界\"}\n\n" +
	"event: token_usage\ndata: {\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}\n\n" +
	"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

func traeResp(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 客户端要流式：必须逐帧转成 OpenAI chat.completion.chunk，并以 [DONE] 收尾。
func TestRelayTraeStream(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &Server{}
	s.relayTrae(rec, traeResp(traeSSE), true, false, protocolOpenAI, "trae/demo", nil, "trae", newUsageCtx("trae/demo"))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("没有 OpenAI chunk 帧：%s", body[:min(len(body), 300)])
	}
	if !strings.Contains(body, "你好") || !strings.Contains(body, "，世界") {
		t.Errorf("正文丢失：%s", body)
	}
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Errorf("结尾不是 data: [DONE]：%q", body[max(0, len(body)-80):])
	}
	// 辅助事件不能漏到客户端
	if strings.Contains(body, "progress_notice") {
		t.Errorf("辅助事件被透传了：%s", body)
	}
}

// 客户端要非流式：把整条上游流聚合成一个 chat.completion。
func TestRelayTraeAggregate(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &Server{}
	s.relayTrae(rec, traeResp(traeSSE), false, false, protocolOpenAI, "trae/demo", nil, "trae", newUsageCtx("trae/demo"))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("聚合结果不是 JSON：%v (%s)", err, rec.Body.String())
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q", got.Object)
	}
	if got.Model != "trae/demo" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Choices) == 0 || got.Choices[0].Message.Content != "你好，世界" {
		t.Errorf("choices = %+v", got.Choices)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.Choices[0].FinishReason)
	}
	if got.Usage["total_tokens"] != float64(3) {
		t.Errorf("usage = %v", got.Usage)
	}
}

// 上游非 2xx 时回的是普通 JSON，原样转出去（别当事件流解析）。
func TestRelayTraeUpstreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &Server{}
	resp := traeResp(`{"code":4008,"message":"quota exceeded"}`)
	resp.StatusCode = http.StatusTooManyRequests
	s.relayTrae(rec, resp, true, false, protocolOpenAI, "trae/demo", nil, "trae", newUsageCtx("trae/demo"))

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "quota exceeded") {
		t.Errorf("正文 = %s", rec.Body.String())
	}
}
