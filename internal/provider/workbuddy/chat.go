package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// Chat 发一个聊天请求，返回原始响应（可能是 JSON 信封或 SSE 流）。
//
// 用于模型可用性探测：只需要知道上游是接受还是拒绝，
// 不解析内容，所以直接把原始字节交回去由调用方判断。
//
// 注意：WorkBuddy 只吃流式，所以 body 里必须带 stream:true；
// 上游拒绝时通常返回 JSON 信封（含 code/msg），成功时返回 SSE。
func (c Customer) Chat(ctx context.Context, body map[string]any) ([]byte, error) {
	if body == nil {
		body = map[string]any{}
	}
	// 强制流式（非流式会被上游拒）
	if _, ok := body["stream"]; !ok {
		body["stream"] = true
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := c.chatBase() + "/v2/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	c.setHeaders(req, true)

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 只读前 4KB：探测只需要看开头有没有 code 字段
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, &chatAuthError{status: resp.StatusCode}
	}
	return raw, nil
}

// chatBase 返回聊天域（与 growth / billing 不同）。
func (c Customer) chatBase() string {
	if c.Global {
		return "https://www.workbuddy.ai"
	}
	return "https://copilot.tencent.com"
}

type chatAuthError struct{ status int }

func (e *chatAuthError) Error() string {
	if e.status == 403 {
		return "凭据无权限（HTTP 403）"
	}
	return "凭据失效（HTTP 401）"
}
