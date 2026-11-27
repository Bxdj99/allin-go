package config

import "testing"

// 每日 Token 保活的小时配置。
//
// 为什么需要它：自动续期只在「快到期」时动手，token 可能刚好在请求边界过期。
// 每天固定时刻全量续一遍，token 始终新鲜（对齐 wb2api 每晚 22:00 的做法）。

func TestKeepaliveHoursDefault(t *testing.T) {
	var sc ScheduleConfig
	got := sc.KeepaliveHoursValue()
	if len(got) != 1 || got[0] != 22 {
		t.Errorf("默认应为 [22]，得到 %v", got)
	}
}

func TestKeepaliveHoursCustom(t *testing.T) {
	v := []int{3, 15}
	sc := ScheduleConfig{KeepaliveHours: &v}
	got := sc.KeepaliveHoursValue()
	if len(got) != 2 || got[0] != 3 || got[1] != 15 {
		t.Errorf("应返回 [3 15]，得到 %v", got)
	}
}

// 空数组表示「关闭每日保活」，不能被当成「没配置」而回落默认值。
func TestKeepaliveHoursEmptyMeansOff(t *testing.T) {
	empty := []int{}
	sc := ScheduleConfig{KeepaliveHours: &empty}
	if got := sc.KeepaliveHoursValue(); len(got) != 0 {
		t.Errorf("空数组应表示关闭，得到 %v", got)
	}
}

// 越界值要过滤掉。
func TestKeepaliveHoursFiltersInvalid(t *testing.T) {
	v := []int{-1, 24, 100, 9}
	sc := ScheduleConfig{KeepaliveHours: &v}
	got := sc.KeepaliveHoursValue()
	if len(got) != 1 || got[0] != 9 {
		t.Errorf("只应保留 9，得到 %v", got)
	}
}

// 全是越界值时等于关闭，不能回落默认。
func TestKeepaliveHoursAllInvalidMeansOff(t *testing.T) {
	v := []int{-5, 99}
	sc := ScheduleConfig{KeepaliveHours: &v}
	if got := sc.KeepaliveHoursValue(); len(got) != 0 {
		t.Errorf("全越界应等于关闭，得到 %v", got)
	}
}
