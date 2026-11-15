package gateway

import (
	"net/http"
	"strings"

	"allin/internal/config"
)

// 常见供应商预设：一键填好 baseUrl 与模型列表，用户只需粘 Key。
//
// 为什么需要：用户反馈「连 DeepSeek 都没有入口」。DeepSeek 官方确实
// 没有 OAuth，但可以少让他手工填 baseUrl / 模型名这些容易错的字段。

// ProviderPreset 是一条预设。
type ProviderPreset struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	BaseURL string   `json:"baseUrl"`
	Models  []string `json:"models"`
	Note    string   `json:"note,omitempty"`
	// NeedsKey 为真表示必须填 API Key 才能用
	NeedsKey bool `json:"needsKey"`
}

// ProviderPresets 是内置预设表。
// 只放「确认过端点」的；不确定的不放，免得用户白折腾。
var ProviderPresets = []ProviderPreset{
	{
		ID: "deepseek", Name: "DeepSeek 官方", Kind: "openai",
		BaseURL: "https://api.deepseek.com/v1",
		Models:  []string{"deepseek-chat", "deepseek-reasoner"},
		Note:    "官方只有 API Key，没有登录入口。到 platform.deepseek.com 生成 Key。",
		NeedsKey: true,
	},
	{
		ID: "siliconflow", Name: "硅基流动 SiliconFlow", Kind: "openai",
		BaseURL: "https://api.siliconflow.cn/v1",
		Models:  []string{"deepseek-ai/DeepSeek-V3", "Qwen/Qwen2.5-72B-Instruct"},
		Note:    "国内聚合平台，注册送额度。",
		NeedsKey: true,
	},
	{
		ID: "openai", Name: "OpenAI", Kind: "openai",
		BaseURL: "https://api.openai.com/v1",
		Models:  []string{"gpt-4o", "gpt-4o-mini", "o3-mini"},
		NeedsKey: true,
	},
	{
		ID: "anthropic", Name: "Anthropic Claude", Kind: "anthropic",
		BaseURL: "https://api.anthropic.com/v1",
		Models:  []string{"claude-sonnet-4-5", "claude-opus-4-1"},
		NeedsKey: true,
	},
	{
		ID: "openrouter", Name: "OpenRouter", Kind: "openai",
		BaseURL: "https://openrouter.ai/api/v1",
		Models:  []string{"anthropic/claude-sonnet-4.5", "google/gemini-2.5-pro"},
		Note:    "一个 Key 用几十家模型，按量计费。",
		NeedsKey: true,
	},
	{
		ID: "tencent", Name: "腾讯混元", Kind: "openai",
		BaseURL: "https://api.hunyuan.cloud.tencent.com/v1",
		Models:  []string{"hunyuan-turbos-latest"},
		NeedsKey: true,
	},
	{
		ID: "zcode", Name: "智谱 GLM Coding", Kind: "openai",
		BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4",
		Models:  []string{"glm-5.3", "glm-4.6"},
		Note:    "编程套餐，按订阅计费。",
		NeedsKey: true,
	},
}

// handlePresets 返回预设列表。
//
//	GET /api/presets
func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	// 标记哪些已经添加过
	type item struct {
		ProviderPreset
		Added bool `json:"added"`
	}
	out := make([]item, 0, len(ProviderPresets))
	for _, p := range ProviderPresets {
		out = append(out, item{ProviderPreset: p, Added: cfg.FindProvider(p.ID) != nil})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "presets": out})
}

// handlePresetApply 用预设创建（或补全）一个供应商。
//
//	POST /api/presets/apply  {"id":"deepseek","apiKey":"sk-xxx"}
func (s *Server) handlePresetApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var in struct {
		ID     string `json:"id"`
		APIKey string `json:"apiKey"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败"})
		return
	}
	in.ID = strings.TrimSpace(in.ID)
	var preset *ProviderPreset
	for i := range ProviderPresets {
		if ProviderPresets[i].ID == in.ID {
			preset = &ProviderPresets[i]
			break
		}
	}
	if preset == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "未知预设: " + in.ID})
		return
	}
	if preset.NeedsKey && strings.TrimSpace(in.APIKey) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": preset.Name + " 需要 API Key"})
		return
	}

	cfg := s.store.Snapshot()
	p := cfg.FindProvider(preset.ID)
	created := false
	if p == nil {
		p = &config.Provider{ID: preset.ID}
		cfg.Providers = append(cfg.Providers, p)
		created = true
	}
	p.Name = preset.Name
	p.Kind = preset.Kind
	p.BaseURL = preset.BaseURL
	if strings.TrimSpace(in.APIKey) != "" && !strings.Contains(in.APIKey, "...") {
		p.APIKey = strings.TrimSpace(in.APIKey)
	}
	// 模型：保留用户已有的，追加预设里没有的
	have := map[string]bool{}
	for _, m := range p.Models {
		have[m] = true
	}
	for _, m := range preset.Models {
		if !have[m] {
			p.Models = append(p.Models, m)
			have[m] = true
		}
	}

	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"created": created,
		"id":      p.ID,
		"models":  len(p.Models),
		"message": "已添加 " + preset.Name + "，可以点「同步上游模型目录」拉取完整列表",
	})
}
