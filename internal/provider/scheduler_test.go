package provider

import (
	"testing"
	"time"
)

// 调度器按「真实 token 消耗」打分，而不是按请求次数。
// 线上证据：202 次请求吃掉 7443 万输入 token，1028 次请求吃掉 2.39 亿 ——
// 次数差 5 倍、消耗接近，只按次数轮转会轻重大小不分。

func newTestScheduler(now *time.Time) *UsageScheduler {
	s := NewUsageScheduler()
	s.now = func() time.Time { return *now }
	return s
}

// 消耗大的账号负载更高。
func TestSchedulerChargeRaisesLoad(t *testing.T) {
	now := time.Now()
	s := newTestScheduler(&now)

	s.Charge("a", 200000) // 一次 20 万 token 的重活
	s.Charge("b", 2000)   // 十次小请求的总和

	if s.Load("a") <= s.Load("b") {
		t.Errorf("重活账号负载应更高：a=%v b=%v", s.Load("a"), s.Load("b"))
	}
}

// 负载按 1 小时半衰期衰减：一小时前的账只剩一半。
func TestSchedulerDecaysOverTime(t *testing.T) {
	now := time.Now()
	s := newTestScheduler(&now)

	s.Charge("a", 100000)
	base := s.Load("a")
	if base < 100000 {
		t.Fatalf("刚记完的负载应约为 100000，得到 %v", base)
	}

	now = now.Add(loadHalfLife)
	half := s.Load("a")
	if half > base*0.55 || half < base*0.45 {
		t.Errorf("半衰期后应约为一半：base=%v half=%v", base, half)
	}

	// 再过 20 个半衰期，应当基本归零（10 个半衰期还有千分之一）
	now = now.Add(20 * loadHalfLife)
	if got := s.Load("a"); got > 1 {
		t.Errorf("长时间后应趋近 0，得到 %v", got)
	}
}

// 选号挑负载最低的。
func TestSchedulerPicksLeastLoaded(t *testing.T) {
	now := time.Now()
	s := newTestScheduler(&now)

	s.Charge("heavy", 500000)
	s.Charge("light", 1000)

	picked, release, ok := s.PickAndReserve([]string{"heavy", "light"})
	if !ok {
		t.Fatal("应能选出账号")
	}
	defer release(0)

	if picked != "light" {
		t.Errorf("应选负载最低的 light，实际 %q", picked)
	}
}

// 在途预留：正在跑的账号要算上预留，避免并发首请求全压同一个号。
func TestSchedulerInFlightReservation(t *testing.T) {
	now := time.Now()
	s := newTestScheduler(&now)

	// 两个都是新号，负载相同
	p1, r1, ok1 := s.PickAndReserve([]string{"a", "b"})
	if !ok1 {
		t.Fatal("第一次应能选出")
	}
	if p1 != "a" {
		t.Fatalf("同负载时应取先者 a，实际 %q", p1)
	}

	// a 现在有在途，下一次应选 b
	p2, r2, ok2 := s.PickAndReserve([]string{"a", "b"})
	if !ok2 {
		t.Fatal("第二次应能选出")
	}
	if p2 != "b" {
		t.Errorf("a 有在途时应选 b，实际 %q", p2)
	}

	r1(0)
	r2(0)

	// 都释放后回到同负载，再次应选 a（顺序优先）
	p3, r3, _ := s.PickAndReserve([]string{"a", "b"})
	if p3 != "a" {
		t.Errorf("在途释放后应回到 a，实际 %q", p3)
	}
	r3(0)
}

// 释放函数幂等；重复调用不会把在途数减成负数。
func TestSchedulerReleaseIdempotent(t *testing.T) {
	now := time.Now()
	s := newTestScheduler(&now)

	_, release, _ := s.PickAndReserve([]string{"a"})
	release(100)
	release(999) // 第二次应被忽略

	if got := s.InFlight("a"); got != 0 {
		t.Errorf("在途数应为 0，得到 %d", got)
	}
	if got := s.Score("a"); got != 100 {
		t.Errorf("分数应为 100（第二次 release 不应再加），得到 %v", got)
	}
}

// 失败请求（tokens=0）也要释放名额，只是不加分。
func TestSchedulerReleaseWithoutTokens(t *testing.T) {
	now := time.Now()
	s := newTestScheduler(&now)

	_, release, _ := s.PickAndReserve([]string{"a"})
	if s.InFlight("a") != 1 {
		t.Fatal("应有 1 个在途")
	}
	release(0)
	if s.InFlight("a") != 0 {
		t.Error("失败也应释放在途名额，否则该号被永久低估")
	}
	if s.Score("a") != 0 {
		t.Errorf("失败不加分，分数应为 0，得到 %v", s.Score("a"))
	}
}

// 未知账号负载为 0，不会挡住新号。
func TestSchedulerUnknownLoadIsZero(t *testing.T) {
	now := time.Now()
	s := newTestScheduler(&now)
	if got := s.Load("never-seen"); got != 0 {
		t.Errorf("未知账号负载应为 0，得到 %v", got)
	}
}

// Reset 清空全部记录。
func TestSchedulerReset(t *testing.T) {
	now := time.Now()
	s := newTestScheduler(&now)
	s.Charge("a", 12345)
	s.Reset()
	if got := s.Load("a"); got != 0 {
		t.Errorf("Reset 后应为 0，得到 %v", got)
	}
}

// Pool 层的封装：ReserveLoad / ChargeLoad / LoadOf 都要能用，
// 且释放后负载与在途都归零。
func TestPoolLoadWiring(t *testing.T) {
	p := NewPool()

	release := p.ReserveLoad("wb", "a1")
	if p.LoadOf("wb", "a1") < inFlightReservation*0.9 {
		t.Errorf("占用后应有在途预留，得到 %v", p.LoadOf("wb", "a1"))
	}
	release(5000)
	// 衰减会带来极小的浮点误差，用容差比较
	if got := p.LoadOf("wb", "a1"); got < 4990 || got > 5000 {
		t.Errorf("释放后负载应约为 5000，得到 %v", got)
	}

	p.ChargeLoad("wb", "a1", 1000)
	if got := p.LoadOf("wb", "a1"); got < 5990 || got > 6000 {
		t.Errorf("累计负载应约为 6000，得到 %v", got)
	}
	// 未知账号不报错
	if got := p.LoadOf("wb", "nope"); got != 0 {
		t.Errorf("未知账号负载应为 0，得到 %v", got)
	}
}
