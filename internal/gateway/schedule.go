package gateway

import (
	"net/http"
	"sort"

	"allin/internal/config"
)

// 自动任务开关的读写。
//
//	GET  /api/schedule  → {autoCheckin, checkinHours}
//	POST /api/schedule  {autoCheckin: bool, checkinHours: [9,21]}
//
// 改完即时生效：service.sh 每 30 秒读一次配置文件。
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"autoCheckin":    cfg.Schedule.AutoCheckinEnabled(),
			"checkinHours":   cfg.Schedule.Hours(),
			"keepaliveHours": cfg.Schedule.KeepaliveHoursValue(),
			"hint":           "自动跑 WorkBuddy 全套任务（签到/连登/出行/抽奖），改完即时生效",
		})
		return

	case http.MethodPost:
		var body struct {
			AutoCheckin    *bool `json:"autoCheckin"`
			CheckinHours   []int `json:"checkinHours"`
			KeepaliveHours []int `json:"keepaliveHours"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
			return
		}
		next := config.ScheduleConfig{
			AutoCheckin:    cfg.Schedule.AutoCheckin,
			CheckinHours:   cfg.Schedule.CheckinHours,
			KeepaliveHours: cfg.Schedule.KeepaliveHours,
		}
		if body.AutoCheckin != nil {
			v := *body.AutoCheckin
			next.AutoCheckin = &v
		}
		if body.CheckinHours != nil {
			// 去重、排序、只留 0-23
			seen := map[int]bool{}
			var hours []int
			for _, h := range body.CheckinHours {
				if h < 0 || h > 23 || seen[h] {
					continue
				}
				seen[h] = true
				hours = append(hours, h)
			}
			sort.Ints(hours)
			if len(hours) == 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "至少要保留一个小时"})
				return
			}
			next.CheckinHours = hours
		}
		if body.KeepaliveHours != nil {
			seen := map[int]bool{}
			var hrs []int
			for _, h := range body.KeepaliveHours {
				if h < 0 || h > 23 || seen[h] {
					continue
				}
				seen[h] = true
				hrs = append(hrs, h)
			}
			sort.Ints(hrs)
			// 允许空数组 = 关闭每日保活
			next.KeepaliveHours = &hrs
		}
		cfg.Schedule = next
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"autoCheckin":    next.AutoCheckinEnabled(),
			"checkinHours":   next.Hours(),
			"keepaliveHours": next.KeepaliveHoursValue(),
			"message":        "已保存，30 秒内生效",
		})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
}
