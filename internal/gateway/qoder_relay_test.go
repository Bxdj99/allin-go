package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// qoderChunk 造一个上游那种 OpenAI 形态的增量帧，并包进 {statusCodeValue, body} 信封。
func qoderFrame(content, finish string) string {
	delta := map[string]any{}
	if content != "" {
		delta["content"] = content
	}
	var choice map[string]any = map[string]any{"index": 0, "delta": delta}
	if finish == "" {
		choice["finish_reason"] = nil
	} else {
		choice["finish_reason"] = finish
	}
	chunk, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-q", "object": "chat.completion.chunk", "created": 1700000000,
		"model": "qmodel_demo", "choices": []any{choice},
	})
	env, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": string(chunk)})
	return "data: " + string(env) + "\n\n"
}

// qoderSSE 是一段典型的 Qoder 上游事件流。
var qoderSSE = qoderFrame("你好", "") + qoderFrame("，世界", "") + qoderFrame("", "stop") + "data: [DONE]\n\n"

func qoderResp(body string, status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 客户端要流式：解帧后逐帧写回标准 OpenAI chunk，并以 [DONE] 收尾。
func TestRelayQoderStream(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &Server{}
	s.relayQoder(rec, qoderResp(qoderSSE, http.StatusOK), true, false, protocolOpenAI, "qoder_cn/qmodel_demo", "qoder_cn", newUsageCtx("qoder_cn/qmodel_demo"))

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
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("结尾不是 [DONE]：%s", body)
	}
	if strings.Contains(body, "statusCodeValue") {
		t.Error("信封字段泄漏给客户端")
	}
}

// 客户端要非流式：整条流聚合成一个 chat.completion。
func TestRelayQoderAggregate(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &Server{}
	s.relayQoder(rec, qoderResp(qoderSSE, http.StatusOK), false, false, protocolOpenAI, "qoder_cn/qmodel_demo", "qoder_cn", newUsageCtx("qoder_cn/qmodel_demo"))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	if got.Object != "chat.completion" || len(got.Choices) != 1 {
		t.Fatalf("聚合结构不对: %+v", got)
	}
	if got.Choices[0].Message.Content == nil || *got.Choices[0].Message.Content != "你好，世界" {
		t.Fatalf("正文聚合不对: %+v", got.Choices[0].Message.Content)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason 不对: %q", got.Choices[0].FinishReason)
	}
}

// 上游直接报错时，普通 JSON 错误体要原样转出去。
func TestRelayQoderUpstreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &Server{}
	s.relayQoder(rec, qoderResp(`{"error":"quota exhausted"}`, http.StatusTooManyRequests),
		true, false, protocolOpenAI, "qoder_cn/qmodel_demo", "qoder_cn", newUsageCtx("qoder_cn/qmodel_demo"))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码应透传，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "quota exhausted") {
		t.Fatalf("错误体没转出去: %s", rec.Body.String())
	}
}
