package gateway

import (
	"context"
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
func (s *Server) startTokenRefresher(ctx context.Context) {
	go func() {
		// 启动后稍等一会儿再第一次巡检，避开启动高峰
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		ticker := time.NewTicker(tokenRefreshInterval)
		defer ticker.Stop()
		for {
			s.refreshExpiringTokens(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
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
				// 还没到提前刷新窗口，但如果已标记需重登，仍要尝试续期
				if !s.pool.NeedRelogin(p.ID, a.ID) {
					continue
				}
			}
			if err := s.refreshOne(ctx, p, a); err == nil {
				changed = true
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
