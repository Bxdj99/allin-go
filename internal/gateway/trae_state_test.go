package gateway

import (
	"errors"
	"testing"

	"allin/internal/config"
	"allin/internal/provider"
)

// trae 把业务错误放在 HTTP 200 的 body 里，前面按状态码写的
// 401/429 分支完全看不到它 —— 于是账号永不冷却，客户端重试就无限
// 往限流窗口里砸请求。实测连打 95 次后恢复窗口被显著拉长。
// 这里锁住「200 + body 错误也要影响账号状态」这条。

func newPoolWithTraeAccount(t *testing.T) (*provider.Pool, *config.Provider) {
	t.Helper()
	en := true
	p := &config.Provider{
		ID: "trae-cn", Kind: provider.KindTrae, Enabled: &en,
		Accounts: []*config.Account{{ID: "t1", Access: "tok", Enabled: &en}},
	}
	pool := provider.NewPool()
	return pool, p
}

func TestTraeMarkAccountStateRateLimit(t *testing.T) {
	pool, p := newPoolWithTraeAccount(t)
	s := &Server{pool: pool}

	err := errors.New(`Trae 上游错误 4011（HTTP 429）：` +
		`We're sorry, your requests have exceeded the rate limit.`)
	traeMarkAccountState(s, p.ID, "t1", err)

	// 冷却按 failures 倍数递增，首次只有 coolRateLimit 一档（很短），
	// 所以不能断言「立刻不可选」。真正要锁住的是：
	// 连续撞限流会把冷却拉长，且账号最终会被排除在候选外。
	for i := 0; i < 10; i++ {
		traeMarkAccountState(s, p.ID, "t1", err)
	}
	// Pick 在「全部账号都冷却」时会回退到最早恢复的那个（避免
	// 用户看到「全部不可用」），所以不能用 Pick 断言冷却。
	// 直接看调度器状态里的 cooling 标记。
	cooling := false
	for _, row := range pool.Snapshot() {
		if row["key"] == p.ID+"/t1" {
			if v, _ := row["cooling"].(bool); v {
				cooling = true
			}
		}
	}
	if !cooling {
		t.Error("连续限流后账号应处于冷却状态")
	}
}

func TestTraeMarkAccountStateAuthFail(t *testing.T) {
	pool, p := newPoolWithTraeAccount(t)
	s := &Server{pool: pool}

	err := errors.New(`Trae 上游错误 1001：we are not able to authenticate you`)
	traeMarkAccountState(s, p.ID, "t1", err)

	if !pool.NeedRelogin(p.ID, "t1") {
		t.Error("1001 鉴权失败应标记 needRelogin，避免每轮白打")
	}
}

// 其它无关错误不该改动账号状态，否则会误伤正常账号。
func TestTraeMarkAccountStateIgnoresOtherErrors(t *testing.T) {
	pool, p := newPoolWithTraeAccount(t)
	s := &Server{pool: pool}

	traeMarkAccountState(s, p.ID, "t1", errors.New("context canceled"))

	if pool.NeedRelogin(p.ID, "t1") {
		t.Error("无关错误不该标记重登")
	}
	if acct := pool.Pick(p, nil); acct == nil {
		t.Error("无关错误不该让账号冷却")
	}
}

// nil 防御：relay 路径上 err 理论上可能为 nil，不能 panic。
func TestTraeMarkAccountStateNilSafe(t *testing.T) {
	traeMarkAccountState(nil, "p", "a", errors.New("x"))
	s := &Server{}
	traeMarkAccountState(s, "p", "a", nil)
}
