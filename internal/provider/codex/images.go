package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Codex 生图（ChatGPT 订阅自带）。
//
// 上游把生图作为 chat 目录之外的独立入口暴露，协议与聊天不同：
//
//	POST https://chatgpt.com/backend-api/codex/images/generations
//	Authorization: Bearer <access>
//	chatgpt-account-id: <account id>
//	originator: codex_cli_rs
//	x-codex-imagegen-request-id: <uuid>
//	x-codex-image-turn-id: <uuid>
//	Accept-Encoding: identity   ← 必须，压缩会让响应体不可用
//
// 请求体 {model, prompt, n, [size, quality, background, output_format]}，
// 响应是 OpenAI images 形状（data[].b64_json 或 data[].url）。
//
// 只有 ChatGPT 登录态（OAuth）账号能用，sk- 形态的 API key 不行。

const (
	ImagesModel            = "gpt-image-2"
	ImagesEndpoint         = "https://chatgpt.com/backend-api/codex/images/generations"
	ImagesMaxResponseBytes = 24 << 20
)

// ImageAccount 是生图用到的账号字段。
type ImageAccount struct {
	Access    string
	AccountID string // 对应 chatgpt-account-id
}

// IsImageModel 判断模型名是否为 codex 生图模型（带不带 codex/ 前缀都认）。
func IsImageModel(model string) bool {
	m := strings.TrimPrefix(strings.TrimSpace(model), "codex/")
	if !strings.HasPrefix(m, "gpt-image-") {
		return false
	}
	rest := strings.TrimPrefix(m, "gpt-image-")
	if rest == "" || len(rest) > 80 {
		return false
	}
	for _, r := range rest {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

// BuildImagePayload 把客户端请求体整理成上游要的形状。
func BuildImagePayload(raw []byte) ([]byte, error) {
	var in struct {
		Model        string          `json:"model"`
		Prompt       json.RawMessage `json:"prompt"`
		N            json.RawMessage `json:"n"`
		Size         *string         `json:"size"`
		Quality      *string         `json:"quality"`
		Background   *string         `json:"background"`
		OutputFormat *string         `json:"output_format"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	prompt, err := promptText(in.Prompt)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("缺少 prompt")
	}
	model := strings.TrimPrefix(strings.TrimSpace(in.Model), "codex/")
	if model == "" {
		model = ImagesModel
	}
	out := map[string]any{"model": model, "prompt": prompt, "n": 1}
	if len(in.N) > 0 && string(in.N) != "null" {
		var n int
		if json.Unmarshal(in.N, &n) != nil {
			return nil, fmt.Errorf("n 必须是整数")
		}
		if n > 4 {
			return nil, fmt.Errorf("n 最多 4")
		}
		if n >= 1 {
			out["n"] = n
		}
	}
	for _, f := range []struct {
		key string
		val *string
	}{{"size", in.Size}, {"quality", in.Quality}, {"background", in.Background}, {"output_format", in.OutputFormat}} {
		if f.val != nil && strings.TrimSpace(*f.val) != "" {
			out[f.key] = *f.val
		}
	}
	return json.Marshal(out)
}

// promptText 兼容 prompt 是字符串或 [{type:text,text:...}] 两种写法。
func promptText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", fmt.Errorf("prompt 必须是字符串")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String(), nil
}

// GenerateImage 调上游生图，返回原始响应体（已是 OpenAI images 形状）。
func GenerateImage(ctx context.Context, acc ImageAccount, payload []byte) ([]byte, error) {
	if strings.TrimSpace(acc.Access) == "" {
		return nil, fmt.Errorf("codex 账号没有 access token")
	}
	if strings.HasPrefix(acc.Access, "sk-") {
		return nil, fmt.Errorf("生图需要 ChatGPT 登录态账号，当前账号是 API key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ImagesEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// 必须 identity：压缩后拿到的不是能直接解析的 JSON
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Authorization", "Bearer "+acc.Access)
	if acc.AccountID != "" {
		req.Header.Set("chatgpt-account-id", acc.AccountID)
	}
	req.Header.Set("originator", Originator)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("x-codex-imagegen-request-id", newUUID())
	req.Header.Set("x-codex-image-turn-id", newUUID())

	client := &http.Client{Timeout: 180 * time.Second, Transport: HTTPClient.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, ImagesMaxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &ImageHTTPError{Status: resp.StatusCode, Body: snippet(body)}
	}
	if !hasImage(body) {
		return nil, fmt.Errorf("上游没有返回图片: %s", snippet(body))
	}
	return body, nil
}

// ImageHTTPError 是上游非 2xx。
type ImageHTTPError struct {
	Status int
	Body   string
}

func (e *ImageHTTPError) Error() string {
	return fmt.Sprintf("Codex 生图上游 HTTP %d: %s", e.Status, e.Body)
}

// hasImage 判断响应里是否真的带图（b64_json 或 url）。
func hasImage(raw []byte) bool {
	var env struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return false
	}
	for _, d := range env.Data {
		if strings.TrimSpace(d.B64) != "" || strings.TrimSpace(d.URL) != "" {
			return true
		}
	}
	return false
}

// newUUID 生成 v4 UUID（上游要求这两个头是 UUID 形态）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
