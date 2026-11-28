package gateway

import (
	"context"
	"log"
	"sync"
	"time"

	"allin/internal/config"
	"allin/internal/provider"
)

// Token 自动刷新。
//
// 之前的问题：RefreshToken 只在登录那一刻用过一次，网关跑起来之后从不刷新。
// 后果是 token 到期（WorkBuddy 大约 7 天、Copilot 30 分钟、Codex 1 小时）
// 之后所有请求都 401，用户只能重新登录。
//
// 这里做一个后台巡检：每 10 分钟扫一遍所有账号，快到期就刷，刷完落盘。
// 刷新失败不立即禁用账号，交给请求路径的换号逻辑处理（可能只是暂时网络问题）。

const (
	// tokenRefreshInterval 巡检间隔
	tokenRefreshInterval = 10 * time.Minute
	// tokenRefreshLead 提前多久刷新（避免请求打到过期边界）
	tokenRefreshLead = 30 * time.Minute
)

var refreshMu sync.Mutex

// startTokenRefresher 启动后台刷新循环。在网关启动时调用一次。
//
// 两条线并行（v1.55.0）：
//   - 每 10 分钟：只刷「快到期」的，作为兜底。
//   - 每天固定时刻（keepaliveHours，默认 22 点）：全量续一遍，
//     让 token 始终新鲜，避免刚好在请求边界过期。
//
// 这样对齐 wb2api 的「每晚 22:00 token 保活」，同时保留细粒度兜底。
func (s *Server) startTokenRefresher(ctx context.Context) {
	go func() {
		// 启动后稍等一会儿再第一次巡检，避开启动高峰
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}

		lastKeepaliveSlot := ""
		ticker := time.NewTicker(tokenRefreshInterval)
		defer ticker.Stop()
		for {
			// 每日全量保活：判断「当前小时命中 keepaliveHours 且今天这小时还没跑过」
			if s.maybeDailyKeepalive(ctx, &lastKeepaliveSlot) {
				// 刚跑完全量，本轮不必再扫快过期的
			} else {
				s.refreshExpiringTokens(ctx)
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// maybeDailyKeepalive 到了设定的整点就做一次全量保活。
//
// 返回 true 表示这一轮已经跑过全量保活。
// slot 用「年月日-小时」做幂等键，保证同一个小时间只跑一次
// （10 分钟 ticker 会在一小时内命中 6 次，不能每次都刷）。
func (s *Server) maybeDailyKeepalive(ctx context.Context, lastSlot *string) bool {
	cfg := s.store.Snapshot()
	if cfg == nil {
		return false
	}
	hours := cfg.Schedule.KeepaliveHoursValue()
	if len(hours) == 0 {
		return false // 空 = 关闭每日保活
	}

	now := time.Now()
	cur := now.Hour()
	hit := false
	for _, h := range hours {
		if h == cur {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}

	slot := now.Format("20060102-15")
	if *lastSlot == slot {
		return false
	}
	*lastSlot = slot

	if !refreshMu.TryLock() {
		return false // 手动保活正在跑
	}
	defer refreshMu.Unlock()

	kctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	log.Printf("每日 Token 保活开始（%s 点档）", slot)
	_, okN, skipN, failN, updates := s.keepaliveAll(kctx, "")
	// 只合并凭据字段，不覆盖保活期间用户改的其它配置
	if err := s.mergeCredentials(updates); err != nil {
		log.Printf("每日保活结果保存失败: %v", err)
	}
	log.Printf("每日 Token 保活完成：成功 %d / 跳过 %d / 失败 %d", okN, skipN, failN)
	return true
}

// refreshExpiringTokens 扫描所有供应商的账号，把快过期的刷掉。
func (s *Server) refreshExpiringTokens(ctx context.Context) {
	if !refreshMu.TryLock() {
		return // 上一轮还没跑完
	}
	defer refreshMu.Unlock()

	cfg := s.store.Snapshot()
	now := time.Now().UnixMilli()
	changed := false

	for _, p := range cfg.Providers {
		if p == nil || !p.IsEnabled() {
			continue
		}
		for _, a := range p.Accounts {
			if a == nil || !a.IsEnabled() || a.Refresh == "" {
				continue
			}
			// Expires 为 0 也可能是因为老配置没存过期时间。
			// 这种情况不能直接跳过：token 可能已经失效，
			// 交给 pool 的 needRelogin 标记判断该不该刷。
			if a.Expires > 0 && a.Expires-now > tokenRefreshLead.Milliseconds() {
				// 还没到提前刷新窗口 —— 跳过。
				//
				// 之前这里还允许「已标 needRelogin 的号继续试」，结果是
				// 被吊销的账号每 10 分钟重试一次（一天 144 次），全是白打。
				// 现在这类账号交给每日全量保活处理（一天一次），
				// 既不漏掉「万一恢复了」，也不刷屏刷请求。
				continue
			}
			if err := s.refreshOne(ctx, p, a); err == nil {
				changed = true
				// 续期成功必须清掉 needRelogin 标记。
				//
				// 不清的话：账号被标「需重登」→ 积分刷新跳过它 →
				// 即使 token 已经换成新的，也永远不再被使用。
				// 线上见过 4 个账号卡在这个状态，明明能跑却一直被跳过。
				if s.pool.NeedRelogin(p.ID, a.ID) {
					s.pool.MarkNeedRelogin(p.ID, a.ID, false)
					log.Printf("续期成功，已清除「需重登」标记: %s/%s", p.ID, a.ID)
				}
			}
		}
	}

	if changed {
		if err := s.store.Save(cfg); err != nil {
			// 落盘失败不致命：内存里已经是最新的，下次请求能用
			_ = err
		}
	}
}

// refreshOne 刷新单个账号的 token，就地更新 a。
func (s *Server) refreshOne(ctx context.Context, p *config.Provider, a *config.Account) error {
	kind := provider.NormalizeKind(p.Kind)
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch kind {
	case provider.KindCopilot:
		return s.refreshCopilot(rctx, a)
	case provider.KindCodex:
		return s.refreshCodex(rctx, a)
	case provider.KindCline:
		return s.refreshCline(rctx, a)
	case provider.KindMiniMax:
		return s.refreshMiniMax(rctx, a)
	case provider.KindWorkBuddy:
		return s.refreshWorkBuddy(rctx, a)
	default:
		// 其它家没有刷新接口（或刷新需要额外参数），跳过
		return errSkipRefresh
	}
}

type skipRefreshErr struct{}

func (skipRefreshErr) Error() string { return "该供应商不支持自动刷新" }

var errSkipRefresh = skipRefreshErr{}
