package gateway

import (
	"testing"
	"time"
)

// 断网时不该每 90 秒都撞一次。
// 实测：11-27 凌晨断网 8 小时，产生 1189 条「积分刷新失败」，全是同一句话。

func resetBackoff() {
	creditsMu.Lock()
	creditsFailStreak = 0
	creditsMu.Unlock()
}

func setStreak(n int) {
	creditsMu.Lock()
	creditsFailStreak = n
	creditsMu.Unlock()
}

func TestBackoffNoFailureIsNormal(t *testing.T) {
	resetBackoff()
	if got := nextRefreshDelay(); got != creditsRefreshInterval {
		t.Errorf("没失败时应用正常间隔 %v，得到 %v", creditsRefreshInterval, got)
	}
}

func TestBackoffGrows(t *testing.T) {
	resetBackoff()
	var prev time.Duration
	for n := 1; n <= 4; n++ {
		setStreak(n)
		got := nextRefreshDelay()
		if n > 1 && got <= prev {
			t.Errorf("第 %d 次失败后间隔应继续增长：%v → %v", n, prev, got)
		}
		prev = got
	}
	resetBackoff()
}

func TestBackoffCapped(t *testing.T) {
	// 失败很多轮也不能无限增长
	setStreak(100)
	if got := nextRefreshDelay(); got != creditsBackoffMax {
		t.Errorf("应封顶在 %v，得到 %v", creditsBackoffMax, got)
	}
	resetBackoff()
}

// 单次成功就该完全恢复，不该记仇。
func TestBackoffResetsOnSuccess(t *testing.T) {
	setStreak(5)
	if nextRefreshDelay() == creditsRefreshInterval {
		t.Fatal("失败 5 次后应已退避")
	}
	resetBackoff()
	if got := nextRefreshDelay(); got != creditsRefreshInterval {
		t.Errorf("恢复后应回到 %v，得到 %v", creditsRefreshInterval, got)
	}
}
