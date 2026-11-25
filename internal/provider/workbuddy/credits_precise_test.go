package workbuddy

import (
	"encoding/json"
	"testing"
)

// 上游同时返回两套余量字段，含义完全不同：
//
//	CapacityRemain      = 资源包总余量（含下个周期未释放的额度）
//	CycleCapacityRemain = 本周期余量，才是当下能用的
//
// 旧实现优先取 CapacityRemain，把「下周期额度」当成「现在可用」，
// 余额早已耗尽的账号显示还有 500+，导致保底与选号全部失效。
// 线上实测（2026-11-25）：8 个账号真实本周期余额只有 0~10，
// 网关却认为都在 500 以上。

func num(s string) json.Number { return json.Number(s) }

// 典型状态：本月 500 分已用满，CapacityRemain 显示 500（下月才释放）。
func TestPreciseCycleExhausted(t *testing.T) {
	a := creditAccount{
		Status:           0,
		CapacitySize:     num("500"),
		CapacityUsed:     num("0"),
		CapacityRemain:   num("500"),
		SizePrecise:      num("500"),
		UsedPrecise:      num("0"),
		RemainPrecise:    num("500"),
		CycleSizePrecise: num("500"),
		CycleUsedPrecise: num("500"),
		CycleRemainPrec:  num("0"),
		CycleStartTime:   "2026-11-01 00:00:00",
		CycleEndTime:     "2026-11-30 23:59:59",
	}

	size, used, remain := a.precise()

	if size != 500 {
		t.Errorf("size = %v, want 500", size)
	}
	if used != 500 {
		t.Errorf("used = %v, want 500（本周期已用满）", used)
	}
	if remain != 0 {
		t.Errorf("remain = %v, want 0（本周期已耗尽，不能把下周期额度算进来）", remain)
	}
}

// 本周期还有余量时，应当如实反映。
func TestPreciseCyclePartial(t *testing.T) {
	a := creditAccount{
		Status:           0,
		CapacitySize:     num("1500"),
		CapacityRemain:   num("1500"),
		SizePrecise:      num("1500"),
		RemainPrecise:    num("1500"),
		CycleSizePrecise: num("1500"),
		CycleUsedPrecise: num("1493"),
		CycleRemainPrec:  num("7"),
	}

	_, _, remain := a.precise()
	if remain != 7 {
		t.Errorf("remain = %v, want 7（本周期真实剩余）", remain)
	}
}

// 没有周期字段的包（如无周期限制的赠送包）退回总余量口径。
func TestPreciseNoCycleFallback(t *testing.T) {
	a := creditAccount{
		Status:         0,
		CapacitySize:   num("100"),
		CapacityUsed:   num("40"),
		CapacityRemain: num("60"),
		SizePrecise:    num("100"),
		UsedPrecise:    num("40"),
		RemainPrecise:  num("60"),
	}

	size, used, remain := a.precise()
	if size != 100 || used != 40 || remain != 60 {
		t.Errorf("precise = (%v,%v,%v), want (100,40,60)", size, used, remain)
	}
}

// 只给了 precise 字段也必须能用（明文变体为空）。
func TestPreciseOnlyPreciseFields(t *testing.T) {
	a := creditAccount{
		Status:           0,
		SizePrecise:      num("200"),
		UsedPrecise:      num("50"),
		RemainPrecise:    num("150"),
		CycleSizePrecise: num("200"),
		CycleUsedPrecise: num("50"),
		CycleRemainPrec:  num("150"),
	}

	size, used, remain := a.precise()
	if size != 200 || used != 50 || remain != 150 {
		t.Errorf("precise = (%v,%v,%v), want (200,50,150)", size, used, remain)
	}
}
