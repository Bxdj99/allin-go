package gateway

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"allin/internal/config"
	"allin/internal/provider"
)

// 虚拟模型路由组（group/<id>）的配置接口。
//
// 背景：Go 版一直有这个能力，但没人能开出来。
//   - provider.Resolve 里已经会递归解析 group/<id> → targets
//   - config.Config 里已经有 Groups 与 Routing.GroupsEnabled
//   - 但没有任何 API、没有面板，groups 是空数组、groupsEnabled 恒为 false
//
// 用户看到的就是「上游有路由，我这边没有」。
// 这个文件把缺的那半截补上：增删改查 + 开关 + 策略。

// handleGroups 是 /api/groups 的入口。
//
//	GET                      列出全部路由组 + 开关状态 + 可选目标清单
//	POST  {action:"save", ...}      新建或更新一个组
//	POST  {action:"delete", id:"x"}  删组
//	POST  {action:"toggle", enabled:true} 开关全局路由组
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.groupsList(w)
	case http.MethodPost:
		var in struct {
			Action   string   `json:"action"`
			Enabled  bool     `json:"enabled"`
			ID       string   `json:"id"`
			Name     string   `json:"name"`
			Strategy string   `json:"strategy"`
			Pick     string   `json:"pick"`
			Targets  []string `json:"targets"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败"})
			return
		}
		switch in.Action {
		case "toggle":
			s.groupsToggle(w, in.Enabled)
		case "delete":
			s.groupsDelete(w, in.ID)
		case "save":
			s.groupsSave(w, in.ID, in.Name, in.Strategy, in.Pick, in.Targets)
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "未知 action: " + in.Action})
		}
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
	}
}

func (s *Server) groupsList(w http.ResponseWriter) {
	cfg := s.store.Snapshot()
	out := make([]map[string]any, 0, len(cfg.Groups))
	for _, g := range cfg.Groups {
		if g == nil {
			continue
		}
		// 每个目标标注是否还能解析出来 —— 面板要能一眼看出哪个目标已经死了
		targets := make([]map[string]any, 0, len(g.Targets))
		for _, t := range g.Targets {
			live := false
			if _, _, _, err := provider.Resolve(cfg, t); err == nil {
				live = true
			}
			targets = append(targets, map[string]any{"ref": t, "live": live})
		}
		out = append(out, map[string]any{
			"id":         g.ID,
			"name":       g.Name,
			"strategy":   g.Strategy,
			"pick":       g.Pick,
			"targets":    targets,
			"enabled":    true,
			"full":       "group/" + g.ID,
			"tokenNotes": groupTargetTokenNote(cfg, g),
			"advice":     strategyAdviceOf(normStrategy(g.Strategy)),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"enabled": cfg.Routing.GroupsEnabled,
		"groups":  out,
		"strategies": []map[string]string{
			{"key": "order", "name": "顺序主备", "desc": "按顺序取第一个能解析的目标，挂了才顺延到下一个"},
			{"key": "round-robin", "name": "轮询均摊", "desc": "每次请求顺延切换目标，防止单号撞限流"},
			{"key": "manual", "name": "手动指定", "desc": "固定用 pick 指定的那个目标"},
		},
		"candidates": groupCandidates(cfg),
		"advices":    strategyAdvices(),
	})
}

// groupCandidates 给出可以加进路由组的全部 provider/model。
// 面板的下拉直接用它，避免用户手打错一个字符就整组失效。
func groupCandidates(cfg *config.Config) []map[string]string {
	out := make([]map[string]string, 0, 64)
	for _, p := range cfg.Providers {
		if p == nil || !p.IsEnabled() {
			continue
		}
		for _, m := range p.Exposed() {
			out = append(out, map[string]string{
				"ref":      p.ID + "/" + m,
				"provider": p.Name,
				"model":    m,
			})
		}
	}
	return out
}

func (s *Server) groupsToggle(w http.ResponseWriter, enabled bool) {
	cfg := s.store.Snapshot()
	cfg.Routing.GroupsEnabled = enabled
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": enabled})
}

func (s *Server) groupsDelete(w http.ResponseWriter, id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 id"})
		return
	}
	cfg := s.store.Snapshot()
	keep := make([]*config.Group, 0, len(cfg.Groups))
	for _, old := range cfg.Groups {
		if old == nil || old.ID == id {
			continue
		}
		keep = append(keep, old)
	}
	cfg.Groups = keep
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) groupsSave(w http.ResponseWriter, id, name, strategy, pick string, targets []string) {
	id = strings.TrimSpace(id)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "id 不能为空"})
		return
	}
	// id 会拼进 "group/<id>"，斜杠和空格会把模型名解析戳坏
	if strings.ContainsAny(id, "/ \t\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "id 不能包含斜杠或空白"})
		return
	}
	switch strategy {
	case "", "order":
		strategy = "order"
	case "round-robin", "manual":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "strategy 只能是 order / round-robin / manual"})
		return
	}

	clean := make([]string, 0, len(targets))
	seen := map[string]bool{}
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		clean = append(clean, t)
	}
	if len(clean) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "至少要有一个目标"})
		return
	}
	// 校验：目标必须真的能解析出来，否则存进去就是一组死链，
	// 用户只会在请求时报「路由组内所有目标都不可用」，查不到源头。
	cfgNow := s.store.Snapshot()
	for _, t := range clean {
		if _, _, _, err := provider.Resolve(cfgNow, t); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":    false,
				"error": "目标 " + t + " 无效：" + err.Error(),
			})
			return
		}
	}
	if strategy == "manual" {
		if pick == "" || !seen[pick] {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": "manual 策略必须 pick 一个 targets 里的目标",
			})
			return
		}
	} else {
		pick = ""
	}

	g := &config.Group{ID: id, Name: strings.TrimSpace(name), Strategy: strategy, Pick: pick, Targets: clean}
	if g.Name == "" {
		g.Name = id
	}

	cfg := s.store.Snapshot()
	replaced := false
	for i, old := range cfg.Groups {
		if old != nil && old.ID == id {
			cfg.Groups[i] = g
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Groups = append(cfg.Groups, g)
	}
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "full": "group/" + id})
}

// ===== 运行期轮询游标 =====
//
// round-robin 需要在多次请求之间记住「下一个该用谁」。
// 只放内存不落盘：重启后从 0 重新开始，对均摊效果没有影响，
// 落盘反而会给配置写入引入无谓的 IO。

var groupRR struct {
	mu       sync.Mutex
	counters map[string]*uint64
}

func groupRRCounter(id string) *uint64 {
	groupRR.mu.Lock()
	defer groupRR.mu.Unlock()
	if groupRR.counters == nil {
		groupRR.counters = map[string]*uint64{}
	}
	c, ok := groupRR.counters[id]
	if !ok {
		var zero uint64
		c = &zero
		groupRR.counters[id] = c
	}
	return c
}

// groupRest 记录失败目标的冷却截止时间。
//
// 失败只让目标「休息」一会儿，不永久拉黑：上游抽一下不该被记住一辈子。
// 冷却期内跳过它，全部都在冷却时不用干等，按原顺序照打。
var groupRest struct {
	mu   sync.Mutex
	dead map[string]time.Time
}

func groupRestKey(groupID, target string) string { return groupID + "|" + target }

func groupResting(groupID, target string) bool {
	groupRest.mu.Lock()
	defer groupRest.mu.Unlock()
	until, ok := groupRest.dead[groupRestKey(groupID, target)]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(groupRest.dead, groupRestKey(groupID, target))
		return false
	}
	return true
}

func groupRestPut(groupID, target string, d time.Duration) {
	groupRest.mu.Lock()
	defer groupRest.mu.Unlock()
	if groupRest.dead == nil {
		groupRest.dead = map[string]time.Time{}
	}
	groupRest.dead[groupRestKey(groupID, target)] = time.Now().Add(d)
}

// groupOrder 按策略给出本次请求的目标尝试顺序。
//
// 注意这里返回的是「顺序」，不是单个目标 —— 顺序主备天然就是重试链，
// 调用方顺着往下试即可，不需要额外的 fallback 逻辑。
func groupOrder(cfg *config.Config, g *config.Group) []string {
	if g == nil || len(g.Targets) == 0 {
		return nil
	}
	// 先按「能不能解析 + 是否在冷却」筛一遍
	alive := make([]string, 0, len(g.Targets))
	for _, t := range g.Targets {
		if groupResting(g.ID, t) {
			continue
		}
		if _, _, _, err := provider.Resolve(cfg, t); err != nil {
			continue
		}
		alive = append(alive, t)
	}
	// 全都在冷却或全解析失败：不干等，按原始顺序照打，让上游给出真实错误
	if len(alive) == 0 {
		return append([]string(nil), g.Targets...)
	}

	switch g.Strategy {
	case "manual":
		// 手动指定：pick 可用就只给这一个，不可用则退回顺序链
		for _, t := range alive {
			if t == g.Pick {
				return []string{t}
			}
		}
		return alive
	case "round-robin":
		c := groupRRCounter(g.ID)
		n := atomic.AddUint64(c, 1) - 1
		start := int(n % uint64(len(alive)))
		out := make([]string, 0, len(alive))
		for i := 0; i < len(alive); i++ {
			out = append(out, alive[(start+i)%len(alive)])
		}
		return out
	default: // order
		return alive
	}
}

// groupTargetsFor 判断 model 是不是 group/<id>，是就按策略算出尝试顺序。
//
// 返回空切片表示「这不是一个路由组」，调用方照常走普通解析。
// 路由组总开关关掉时也返回空 —— 关掉就是完全不生效，
// 而不是「静默按默认策略跑」，否则用户关了开关却还在分流，只会更懵。
func groupTargetsFor(cfg *config.Config, model string) ([]string, string) {
	if cfg == nil || !strings.HasPrefix(model, "group/") {
		return nil, ""
	}
	if !cfg.Routing.GroupsEnabled {
		return nil, ""
	}
	gid := strings.TrimPrefix(model, "group/")
	g := cfg.FindGroup(gid)
	if g == nil {
		return nil, ""
	}
	return groupOrder(cfg, g), gid
}

// groupRestCooldown 给出不同错误码的休息时长。
//
// 分开的理由：限流（429）是「等一会儿就好」，冷却短；
// 5xx 是上游自己抖动，短歇就够；401 是账号真的废了，
// 歇久点，免得每个请求都拿一个必败的账号去打一遍。
func groupRestCooldown(status int) time.Duration {
	switch {
	case status == 401:
		return 10 * time.Minute
	case status == 429:
		return 60 * time.Second
	default:
		return 30 * time.Second
	}
}
