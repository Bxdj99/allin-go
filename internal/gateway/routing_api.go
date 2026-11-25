package gateway

import (
	"net/http"

	"allin/internal/config"
)

// 选号规则面板接口。
//
//	GET  /api/routing-rules?provider=workbuddy   读规则 + 账号列表
//	POST /api/routing-rules                      改规则
//
// 为什么要做面板：账号选择直接决定积分消耗 ——
// 换号会让上游前缀缓存失效（整段输入从约 10% 计价变全价），
// 实测同一会话 54.9 万 token，换不换号差 10 倍。
// 这个取舍必须让用户自己看见并选择，不能藏在配置里。

type routingRule struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Desc    string `json:"desc"`
	Pros    string `json:"pros"`
	Cons    string `json:"cons"`
	Risk    string `json:"risk"`
}

// routingRules 是四种策略的说明（+ 优先账号）。
func routingRules() []routingRule {
	return []routingRule{
		{
			Key:  "preferredAccountId",
			Name: "优先账号（推荐）",
			Desc: "指定一个账号，只要可用就一直用它",
			Pros: "会话前缀保持一致，缓存能长期命中；实测比多账号分摊省约 10 倍",
			Cons: "该账号一旦耗尽就要换，届时缓存重建一次；余额消耗集中在一个号上",
			Risk: "low",
		},
		{
			Key:  "sticky",
			Name: "会话绑定",
			Desc: "同一段对话钉在一个账号上，对话结束才可能换",
			Pros: "默认策略，兼顾缓存与分散；多数场景够用",
			Cons: "新会话会挑不同的号，多会话并行时缓存命中不如「优先账号」稳定",
			Risk: "low",
		},
		{
			Key:  "expiry",
			Name: "到期优先",
			Desc: "优先用完最早过期的额度",
			Pros: "不会让快到期的额度白白作废",
			Cons: "经常在账号间跳，缓存命中率低，单次成本可能更高",
			Risk: "mid",
		},
		{
			Key:  "balance",
			Name: "用量均衡",
			Desc: "优先用完剩余比例最低的账号，让各号消耗趋同",
			Pros: "余额分布均匀，不会出现「一个号见底、一堆号闲着」",
			Cons: "每个请求都可能换号，缓存几乎无法复用，长会话代价最大",
			Risk: "mid",
		},
		{
			Key:  "rotate",
			Name: "轮询",
			Desc: "每个请求换一个账号，纯粹均摊",
			Pros: "负载最均匀，适合大量短请求",
			Cons: "**缓存完全用不上**，每次都按全价计费 —— 长会话最贵的选择",
			Risk: "high",
		},
	}
}

// handleRoutingRules 读写某个供应商的选号规则。
func (s *Server) handleRoutingRules(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()

	pid := r.URL.Query().Get("provider")
	if pid == "" {
		pid = "workbuddy"
	}

	switch r.Method {
	case http.MethodGet:
		prov := cfg.FindProvider(pid)
		if prov == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "找不到供应商 " + pid})
			return
		}
		type accBrief struct {
			ID      string  `json:"id"`
			Name    string  `json:"name"`
			Enabled bool    `json:"enabled"`
			Credits float64 `json:"credits"`
			HasCred bool    `json:"hasCredits"`
			State   string  `json:"state"`
		}
		var accs []accBrief
		states := map[string]string{}
		if snap := s.pool.Snapshot(); snap != nil {
			for _, row := range snap {
				k, _ := row["key"].(string)
				if st, ok := row["lastFailure"].(string); ok && st != "" {
					states[k] = st
				}
				if cool, ok := row["cooling"].(bool); ok && cool {
					states[k] = "冷却中"
				}
			}
		}
		for _, a := range prov.Accounts {
			if a == nil {
				continue
			}
			b := accBrief{ID: a.ID, Name: a.Name, Enabled: a.IsEnabled()}
			if v, ok := s.pool.CreditsOf(pid, a.ID); ok {
				b.Credits, b.HasCred = v, true
			}
			if st, ok := states[pid+"/"+a.ID]; ok {
				b.State = st
			}
			accs = append(accs, b)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"provider":      pid,
			"routeMode":     prov.RouteModeOrDefault(),
			"preferred":     prov.PreferredAccountID,
			"accounts":      accs,
			"rules":         routingRules(),
			"note":          "选号规则直接决定积分消耗：换号会让上游前缀缓存失效，整段输入从约 10% 计价变全价。",
		})

	case http.MethodPost:
		var in struct {
			Provider  string `json:"provider"`
			RouteMode string `json:"routeMode"`
			Preferred string `json:"preferred"`
			// SetPreferred 显式区分「没传」与「传空表示清除」
			SetPreferred bool `json:"setPreferred"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
			return
		}
		target := pid
		if in.Provider != "" {
			target = in.Provider
		}
		prov := cfg.FindProvider(target)
		if prov == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "找不到供应商 " + target})
			return
		}

		if in.RouteMode != "" {
			switch in.RouteMode {
			case config.RouteSticky, config.RouteRotate, config.RouteBalance, config.RouteExpiry:
				prov.RouteMode = in.RouteMode
			default:
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"ok": false, "error": "routeMode 只能是 sticky / rotate / balance / expiry",
				})
				return
			}
		}
		if in.SetPreferred {
			// 传了就必须是真实存在的账号，避免打错 id 后静默失效
			if in.Preferred != "" {
				found := false
				for _, a := range prov.Accounts {
					if a != nil && a.ID == in.Preferred {
						found = true
						break
					}
				}
				if !found {
					writeJSON(w, http.StatusBadRequest, map[string]any{
						"ok": false, "error": "优先账号不存在: " + in.Preferred,
					})
					return
				}
			}
			prov.PreferredAccountID = in.Preferred
		}

		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
			return
		}
		// 会话绑定表按账号记的，规则改了应当重来，避免旧绑定干扰新规则。
		s.pool.ResetSessions()

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"routeMode": prov.RouteModeOrDefault(),
			"preferred": prov.PreferredAccountID,
			"message":   "已保存并生效",
		})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
	}
}
