package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Trae 模型目录同步。
//
// 上游：POST {MODEL_HOST}/api/ide/v1/batch_get_detail_param
//
// 请求体（照抄官方 IDE 客户端的查询参数）：
//
//	{"functions":["chat_v3","solo_work_lite","solo_agent","solo_agent_lite","solo_coder"],
//	 "agent_type":"","current_config_info":{"config_name":"","is_custom_model":false},
//	 "mode_type":0,"access_type":0,"ab_force_vids":"","ab_autotest_advanced_mode":0,
//	 "show_custom_model":true}
//
// 响应里 function_configs[].config_info_list[] 是模型配置列表，
// 每个配置有 config_name（就是我们要的模型名）。
//
// 之前 Trae 的模型列表是硬编码的占位值（claude-3.5-sonnet / gpt-4o），
// 那两家模型 Trae CN 根本不提供，调不通。现在改成真拉。

// modelQueryFunctions 是查询时声明的功能族，与官方 IDE 保持一致。
var modelQueryFunctions = []string{
	"chat_v3", "solo_work_lite", "solo_agent", "solo_agent_lite", "solo_coder",
}

// ModelEntry 是一条模型配置。
type ModelEntry struct {
	Name     string `json:"name"` // config_name，调用时用这个
	Function string `json:"function,omitempty"`
	Note     string `json:"note,omitempty"`
}

// FetchModels 拉取账号可用的模型列表。
func FetchModels(ctx context.Context, acc *Account) ([]ModelEntry, error) {
	if acc == nil || acc.Token == "" {
		return nil, fmt.Errorf("账号缺少访问令牌")
	}

	// 第一路：批量查询（官方 IDE 走这个）
	entries, err := batchDetail(ctx, acc)
	if err == nil && len(entries) > 0 {
		return entries, nil
	}

	// 第二路：逐个 function 查（批量失败时的兜底，与 All-in 的做法一致）
	var out []ModelEntry
	seen := map[string]bool{}
	var lastErr error
	for _, fn := range modelQueryFunctions {
		list, e := perFunction(ctx, acc, fn)
		if e != nil {
			lastErr = e
			continue
		}
		for _, m := range list {
			if m.Name == "" || seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("没有拉到任何模型")
}

// batchDetail 调用批量查询端点。
func batchDetail(ctx context.Context, acc *Account) ([]ModelEntry, error) {
	body := map[string]any{
		"functions":     modelQueryFunctions,
		"agent_type":    "",
		"current_config_info": map[string]any{
			"config_name":      "",
			"is_custom_model":  false,
		},
		"mode_type":               0,
		"access_type":             0,
		"ab_force_vids":           "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":       true,
	}
	raw, err := modelRequest(ctx, acc, "/api/ide/v1/batch_get_detail_param", body)
	if err != nil {
		return nil, err
	}
	return parseFunctionConfigs(raw)
}

// perFunction 按单个 function 查询。
func perFunction(ctx context.Context, acc *Account, fn string) ([]ModelEntry, error) {
	body := map[string]any{
		"function":             fn,
		"config_names":         nil,
		"need_prompt":          false,
		"current_config_info":  nil,
		"poly_prompt":          true,
		"mode_type":            nil,
		"agent_type":           nil,
	}
	raw, err := modelRequest(ctx, acc, "/api/ide/v1/get_detail_param", body)
	if err != nil {
		return nil, err
	}
	list, err := parseFunctionConfigs(raw)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Function == "" {
			list[i].Function = fn
		}
	}
	return list, nil
}

// parseFunctionConfigs 从响应里抠出 config_info_list。
func parseFunctionConfigs(raw []byte) ([]ModelEntry, error) {
	var env struct {
		Code            int               `json:"code"`
		Msg             string            `json:"msg"`
		FunctionConfigs []json.RawMessage `json:"function_configs"`
		Data            struct {
			FunctionConfigs []json.RawMessage `json:"function_configs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("模型目录解析失败: %w", err)
	}
	if env.Code != 0 && env.Code != 200 {
		return nil, fmt.Errorf("上游返回 code=%d msg=%s", env.Code, env.Msg)
	}
	groups := env.FunctionConfigs
	if len(groups) == 0 {
		groups = env.Data.FunctionConfigs
	}

	var out []ModelEntry
	seen := map[string]bool{}
	for _, g := range groups {
		var group struct {
			Function       string `json:"function"`
			ConfigInfoList []struct {
				ConfigName    string `json:"config_name"`
				ModelName     string `json:"model_name"`
				DisplayName   string `json:"display_name"`
				IsCustomModel bool   `json:"is_custom_model"`
			} `json:"config_info_list"`
		}
		if json.Unmarshal(g, &group) != nil {
			continue
		}
		for _, c := range group.ConfigInfoList {
			name := strings.TrimSpace(c.ConfigName)
			if name == "" {
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			note := c.DisplayName
			if note == "" {
				note = c.ModelName
			}
			out = append(out, ModelEntry{Name: name, Function: group.Function, Note: note})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// modelRequest 发一次模型目录请求。
func modelRequest(ctx context.Context, acc *Account, path string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://trae-api-cn.mchost.guru"+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range Headers(acc) {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second, Transport: HTTPClient.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("凭据失效（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(raw), 120))
	}
	return raw, nil
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
