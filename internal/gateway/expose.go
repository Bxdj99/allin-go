package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"allin/internal/provider"
)

// ---------- 模型筛选（exposedModels） ----------

// handleProviderExpose 保存「对外暴露哪些模型」的勾选结果。
//
//	POST /api/providers/models/expose  {"providerId":"x","models":["a","b"]}
//
// 语义（与 config.Provider.ExposedModels 一致）：
//   - 勾选结果存进 exposedModels，它只影响 /v1/models 与模型名解析，不改动 models 本体
//     （目录同步仍然往 models 里追加，不会把用户勾选冲掉）。
//   - models 为空数组 → 等价于「一个都不暴露」，落盘为哨兵值（见 noExposedSentinel）。
//   - models 全选 → 落盘为显式全量列表，此时以后同步进来的新模型默认不暴露。
//     用户想恢复「永远全部暴露」，需要手工把配置里的 exposedModels 删掉。
//   - 传进来的模型必须存在于 provider.models，否则 400（防止面板拿着过期数据覆盖配置）。
func (s *Server) handleProviderExpose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "只支持 POST"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取失败: " + err.Error()})
		return
	}
	var in struct {
		ProviderID string   `json:"providerId"`
		Models     []string `json:"models"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
		return
	}
	pid := strings.TrimSpace(in.ProviderID)
	if pid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "providerId 不能为空"})
		return
	}

	cfg := s.store.Snapshot()
	p := cfg.FindProvider(pid)
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "供应商不存在: " + pid})
		return
	}

	have := make(map[string]bool, len(p.Models))
	for _, m := range p.Models {
		have[m] = true
	}
	picked := make([]string, 0, len(in.Models))
	seen := make(map[string]bool, len(in.Models))
	var unknown []string
	for _, m := range in.Models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		if !have[m] {
			unknown = append(unknown, m)
			continue
		}
		picked = append(picked, m)
	}
	if len(unknown) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":      false,
			"error":   "这些模型不在该供应商的 models 里（可能刚同步过目录，请刷新后重试）",
			"unknown": unknown,
		})
		return
	}

	// 按 provider.models 的顺序落盘，读起来稳定，也便于人工看配置文件
	ordered := make([]string, 0, len(picked))
	if len(picked) == 0 {
		// 全不选：存哨兵值（空切片会被 omitempty 吃掉，退化成「全部暴露」）
		ordered = append(ordered, noExposedSentinel)
	} else if len(picked) == len(p.Models) {
		// 全选：不存字段，保持「自动全暴露」语义。
		// 这样以后同步进来的新模型默认可用，用户不用每次回来重新勾。
		p.ExposedModels = nil
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "providerId": pid,
			"exposedModels": nil, "exposedCount": len(p.Models),
			"modelTotal": len(provider.ModelList(cfg)),
			"note":       "已全选，之后同步进来的新模型会自动暴露",
		})
		return
	} else {
		want := make(map[string]bool, len(picked))
		for _, m := range picked {
			want[m] = true
		}
		for _, m := range p.Models {
			if want[m] {
				ordered = append(ordered, m)
			}
		}
	}
	p.ExposedModels = ordered

	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"providerId":    pid,
		"exposedModels": p.ExposedModels,
		"exposedCount":  len(p.ExposedModels),
		"modelTotal":    len(provider.ModelList(cfg)),
	})
}

// noExposedSentinel 是「全部不暴露」的占位值。
//
// 为什么需要它：ExposedModels 为空表示「全部暴露」（老配置兼容），
// 而 omitempty 会把空切片写成「字段消失」，两者在 JSON 层无法区分。
// 存一个不可能存在的模型名，config.Provider.Exposed() 过滤掉它即得到空列表。
const noExposedSentinel = "\x00none"
