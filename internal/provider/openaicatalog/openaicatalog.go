// Package openaicatalog 拉取标准 OpenAI 兼容供应商的模型列表。
//
// 适用面：所有 kind=openai 的上游（DeepSeek / OpenAI / OpenRouter / 硅基流动 …），
// 它们都实现了同一个端点：
//
//	GET {baseURL}/models  →  {"object":"list","data":[{"id":"gpt-4o", ...}, ...]}
//
// 只需要 id（网关的 models 字段只存名字）；其余字段各家五花八门，不解析。
//
// 有两个现实差异要兜住：
//  1. 分页：OpenRouter 之类返回的对象里可能带 "has_more": true 与 data 末尾的
//     "next_cursor"，看需求实现前先按主流实现只读第一页——绝大多数供应商一次给全。
//     这里只在响应里出现明确的 next 游标时才续页，避免对不支持的供应商发无意义请求。
//  2. schema：少数实现把列表直接放在 "models" 或裸数组里。按候选键依次尝试。
package openaicatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultHTTPClient 目录请求用的客户端。超时不宜太长：同步接口是用户点一次等一次。
var DefaultHTTPClient = &http.Client{Timeout: 20 * time.Second}

// maxPages 续页上限，防止上游游标异常导致死循环。
const maxPages = 5

// maxBodyBytes 单页响应上限。
const maxBodyBytes = 8 << 20

// JoinModelsURL 由 baseURL 拼出模型列表地址。
//
// 兼容几种常见写法：
//
//	https://api.deepseek.com/v1      → …/v1/models
//	https://api.openai.com/v1/      → …/v1/models
//	https://openrouter.ai/api/v1    → …/api/v1/models
//	https://api.openai.com           → …/v1/models（补 /v1）
//
// 已经以 /models 结尾的保持原样（用户可能填了完整端点）。
func JoinModelsURL(baseURL string) (string, error) {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		return "", fmt.Errorf("baseUrl 为空")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("baseUrl 非法: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("baseUrl 缺少主机名")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("baseUrl 协议非法: %s", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("baseUrl 不允许带用户信息")
	}

	path := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/models"):
		// 已经是完整端点
	case strings.HasSuffix(path, "/v1"):
		path += "/models"
	case path == "":
		path = "/v1/models"
	default:
		path += "/v1/models"
	}
	u.Path = path
	// 保留查询串（个别供应商要靠它区分子账号），但不追加自己的参数。
	return u.String(), nil
}

// Fetch 拉取模型 id 列表（按上游顺序去重）。
//
// apiKey 为空时不发 Authorization 头（部分自建服务不鉴权）。
func Fetch(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	return FetchWith(ctx, DefaultHTTPClient, baseURL, apiKey)
}

// FetchWith 同 Fetch，但可指定客户端（测试注入用）。
func FetchWith(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]string, error) {
	if client == nil {
		client = DefaultHTTPClient
	}
	endpoint, err := JoinModelsURL(baseURL)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, 32)
	seen := make(map[string]bool, 32)
	cursor := ""
	for page := 0; page < maxPages; page++ {
		pageURL := endpoint
		if cursor != "" {
			sep := "?"
			if strings.Contains(pageURL, "?") {
				sep = "&"
			}
			pageURL += sep + "after_id=" + url.QueryEscape(cursor)
		}
		data, next, err := fetchPage(ctx, client, pageURL, apiKey)
		if err != nil {
			return nil, err
		}
		for _, id := range data {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("上游返回了空的模型列表")
	}
	return out, nil
}

// modelsEnvelope 是响应解析形态。data / models 二选一，数组也可能直接挂在根上。
type modelsEnvelope struct {
	Object  string          `json:"object"`
	Data    json.RawMessage `json:"data"`
	Models  json.RawMessage `json:"models"`
	HasMore bool            `json:"has_more"`
	Next    string          `json:"next_cursor"`
	// 部分实现把游标挂在 data 里，这里只能整段读出来再处理。
}

// entry 是列表元素：常见是对象 {"id": "..."}，也有实现直接给字符串。
type entry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Cursor string `json:"next_cursor"`
}

// fetchPage 取一页，返回 id 列表与下一页游标（空表示没有下一页）。
func fetchPage(ctx context.Context, client *http.Client, endpoint, apiKey string) ([]string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "allin-go/1.0")
	if k := strings.TrimSpace(apiKey); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if rerr != nil {
		return nil, "", fmt.Errorf("读取响应失败: %w", rerr)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, "", fmt.Errorf("鉴权失败（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(strings.ReplaceAll(string(raw), "\n", " "))
		if len(snippet) > 120 {
			snippet = snippet[:120] + "…"
		}
		return nil, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}

	var env modelsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, "", fmt.Errorf("响应不是合法 JSON: %w", err)
	}

	// 定位模型数组：data → models → 根数组。
	blob := env.Data
	if !isJSONArray(blob) {
		blob = env.Models
		if !isJSONArray(blob) {
			blob = raw // 根上直接是数组
		}
	}
	items, err := parseEntries(blob)
	if err != nil {
		return nil, "", err
	}

	next := strings.TrimSpace(env.Next)
	if next == "" && env.HasMore && len(items) > 0 {
		// 游标在末元素上（OpenRouter 风格）：只有明确 has_more 才认。
		next = strings.TrimSpace(items[len(items)-1].Cursor)
	}

	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids, next, nil
}

// isJSONArray 判断原始 JSON 是不是以 [ 开头。
func isJSONArray(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return strings.HasPrefix(t, "[")
}

// parseEntries 解析列表元素，兼容对象与裸字符串两种形态。
func parseEntries(raw json.RawMessage) ([]entry, error) {
	var objs []entry
	if err := json.Unmarshal(raw, &objs); err == nil {
		return objs, nil
	}
	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		out := make([]entry, 0, len(strs))
		for _, s := range strs {
			out = append(out, entry{ID: s})
		}
		return out, nil
	}
	return nil, fmt.Errorf("无法识别的模型列表结构")
}
