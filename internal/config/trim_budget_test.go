package config

import "testing"

// 上下文裁剪预算与开关。
//
// 默认阈值是 95 万（上游硬上限留 10% 余量），只在触顶时才裁。
// 但线上常见「单次 20 万 × 一天几千次」—— 总量早爆了却永远触发不到裁剪，
// 所以必须让用户能把预算调低。

func TestTrimBudgetDefault(t *testing.T) {
	s := &Server{}
	if got := s.TrimBudgetValue(); got != 0 {
		t.Errorf("未配置时应返回 0（表示用默认阈值），得到 %d", got)
	}
}

func TestTrimBudgetCustom(t *testing.T) {
	v := 200000
	s := &Server{TrimBudget: &v}
	if got := s.TrimBudgetValue(); got != 200000 {
		t.Errorf("got %d, want 200000", got)
	}
}

// 负数/0 视同未配置，避免把预算设成 0 导致请求被裁空。
func TestTrimBudgetNonPositiveFallsBack(t *testing.T) {
	for _, v := range []int{0, -1, -99999} {
		budget := v
		s := &Server{TrimBudget: &budget}
		if got := s.TrimBudgetValue(); got != 0 {
			t.Errorf("预算 %d 应视为未配置，得到 %d", v, got)
		}
	}
}

// 开关默认开启。
func TestTrimEnabledDefault(t *testing.T) {
	s := &Server{}
	if !s.TrimEnabledOn() {
		t.Error("裁剪默认应开启")
	}
}

func TestTrimEnabledOff(t *testing.T) {
	off := false
	s := &Server{TrimEnabled: &off}
	if s.TrimEnabledOn() {
		t.Error("显式关闭后应为 false")
	}
}
