package gateway

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
)

// 工具使用统计。
//
// 目的：回答「哪些工具是纯成本」。
//
// 背景：工具定义每轮都发，且不吃缓存折扣（在报文最前面，但它本身是最大的固定前缀）。
// 实测用户挂了 182 个工具 ≈ 4.2 万 token，占单次输入 35%。
// 而历史里真正被调用过的往往只有个位数 —— 那剩下 170 多个就是纯成本。
//
// 统计口径：只扫请求里 messages 的 tool_calls / tool_use，
// 不去解析响应流（那会拖慢转发）。这足以反映「实际用到的工具」。

type toolUsageStat struct {
	declared atomic.Int64 // 被声明（出现在 tools 数组里）的请求数
	used     atomic.Int64 // 被实际调用的次数
}

var (
	toolUsageMu    sync.RWMutex
	toolUsageTable = map[string]*toolUsageStat{} // key: 工具名
	// 最近一次请求声明的工具集合，用于算「声明了但没用过」
	lastDeclaredMu sync.RWMutex
	lastDeclared   = map[string]bool{}
)

func toolStat(name string) *toolUsageStat {
	toolUsageMu.RLock()
	st := toolUsageTable[name]
	toolUsageMu.RUnlock()
	if st != nil {
		return st
	}
	toolUsageMu.Lock()
	defer toolUsageMu.Unlock()
	if st = toolUsageTable[name]; st == nil {
		st = &toolUsageStat{}
		toolUsageTable[name] = st
	}
	return st
}

// recordToolsDeclared 记录一次请求声明了哪些工具。
func recordToolsDeclared(names []string) {
	if len(names) == 0 {
		return
	}
	for _, n := range names {
		if n == "" {
			continue
		}
		toolStat(n).declared.Add(1)
	}
	toolUsageMu.RLock()
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			set[n] = true
		}
	}
	toolUsageMu.RUnlock()
	lastDeclaredMu.Lock()
	lastDeclared = set
	lastDeclaredMu.Unlock()
}

// recordToolsUsed 记录一次请求历史里用到的工具。
func recordToolsUsed(names []string) {
	for _, n := range names {
		if n == "" {
			continue
		}
		toolStat(n).used.Add(1)
	}
}

// extractToolNames 从请求体里抽出两样东西：
//   - declared：tools 数组里声明的工具名
//   - used：messages 历史里真正调用过的工具名
//
// 同时兼容 OpenAI（tool_calls[].function.name）与
// Anthropic（content[].type==tool_use 的 name）。
func extractToolNames(body []byte) (declared, used []string) {
	if len(body) == 0 {
		return nil, nil
	}
	var root struct {
		Tools    []json.RawMessage `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil {
		return nil, nil
	}

	seenDecl := make(map[string]bool, len(root.Tools))
	for _, t := range root.Tools {
		var probe struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
			Name string `json:"name"` // Anthropic 形态直接平铺
		}
		if json.Unmarshal(t, &probe) != nil {
			continue
		}
		name := probe.Function.Name
		if name == "" {
			name = probe.Name
		}
		if name != "" && !seenDecl[name] {
			seenDecl[name] = true
			declared = append(declared, name)
		}
	}

	seenUse := make(map[string]bool)
	for _, m := range root.Messages {
		var msg struct {
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if n := tc.Function.Name; n != "" && !seenUse[n] {
				seenUse[n] = true
				used = append(used, n)
			}
		}
		// Anthropic 形态
		if len(msg.Content) > 0 {
			var blocks []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(msg.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "tool_use" && b.Name != "" && !seenUse[b.Name] {
						seenUse[b.Name] = true
						used = append(used, b.Name)
					}
				}
			}
		}
	}
	return declared, used
}

// recordToolTraffic 统计一次请求的工具使用情况。
// 只做只读扫描，不改请求体。
func recordToolTraffic(body []byte) {
	d, u := extractToolNames(body)
	recordToolsDeclared(d)
	recordToolsUsed(u)
}

// ToolUsageRow 是导出给面板的一行。
type ToolUsageRow struct {
	Name     string `json:"name"`
	Declared int64  `json:"declared"` // 有多少次请求声明了它
	Used     int64  `json:"used"`     // 被实际调用了多少次
	// Idle 为 true 表示「声明过但从没被用过」—— 纯成本。
	Idle bool `json:"idle,omitempty"`
}

// toolUsageRows 导出工具使用榜，按「声明次数 × 未使用」优先排。
func toolUsageRows() []ToolUsageRow {
	toolUsageMu.RLock()
	defer toolUsageMu.RUnlock()
	rows := make([]ToolUsageRow, 0, len(toolUsageTable))
	for name, st := range toolUsageTable {
		d, u := st.declared.Load(), st.used.Load()
		rows = append(rows, ToolUsageRow{Name: name, Declared: d, Used: u, Idle: u == 0 && d > 0})
	}
	sort.Slice(rows, func(i, j int) bool {
		// 从未用过的排前面（那是纯成本），其次按声明次数
		if rows[i].Idle != rows[j].Idle {
			return rows[i].Idle
		}
		if rows[i].Declared != rows[j].Declared {
			return rows[i].Declared > rows[j].Declared
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

// toolUsageSummary 是工具的汇总数字。
type toolUsageSummary struct {
	Total    int   `json:"total"`    // 声明过的工具总数
	UsedEver int   `json:"usedEver"` // 至少用过一次的工具数
	Idle     int   `json:"idle"`     // 声明过但从没用过
	Requests int64 `json:"requests"` // 统计覆盖的请求数
}

var toolUsageRequests atomic.Int64

func toolUsageSummaryOf() toolUsageSummary {
	toolUsageMu.RLock()
	defer toolUsageMu.RUnlock()
	var s toolUsageSummary
	s.Total = len(toolUsageTable)
	for _, st := range toolUsageTable {
		if st.used.Load() > 0 {
			s.UsedEver++
		} else if st.declared.Load() > 0 {
			s.Idle++
		}
	}
	s.Requests = toolUsageRequests.Load()
	return s
}

// handleToolUsage 暴露工具使用统计。
//
//	GET  /api/tools/usage    读统计
//	POST /api/tools/usage    清零
func (s *Server) handleToolUsage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"summary": toolUsageSummaryOf(),
			"rows":    toolUsageRows(),
			"hint":    "idle=true 表示声明过但从没被调用过 —— 每轮都在为它付 token。考虑在客户端关掉对应 MCP 或插件。",
		})
	case http.MethodPost:
		toolUsageMu.Lock()
		toolUsageTable = map[string]*toolUsageStat{}
		toolUsageMu.Unlock()
		toolUsageRequests.Store(0)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已清零"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
	}
}
