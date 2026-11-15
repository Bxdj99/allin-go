package gateway

import (
	"context"
	"time"

	"allin/internal/config"
	"allin/internal/provider/cline"
	"allin/internal/provider/codex"
	"allin/internal/provider/copilot"
	"allin/internal/provider/minimax"
)

// 各家 token 刷新的适配层。
//
// 各包的 Account 结构不同，这里做一次「config.Account ↔ 各家 Account」的搬运：
// 刷新前把凭据塞进去，刷新后把新值搬回来。任何一家刷新失败都不影响其它家。

func (s *Server) refreshCopilot(ctx context.Context, a *config.Account) error {
	ca := &copilot.Account{Access: a.Access, Refresh: a.Refresh, Expires: a.Expires, Name: a.Name}
	if err := copilot.RefreshToken(ctx, ca); err != nil {
		return err
	}
	if ca.Access != "" {
		a.Access = ca.Access
	}
	if ca.Refresh != "" {
		a.Refresh = ca.Refresh
	}
	if ca.Expires > 0 {
		a.Expires = ca.Expires
	}
	return nil
}

func (s *Server) refreshCodex(ctx context.Context, a *config.Account) error {
	ca := &codex.Account{Access: a.Access, Refresh: a.Refresh, Expires: a.Expires, AccountID: a.UID}
	if err := codex.Refresh(ctx, ca, "scheduled"); err != nil {
		return err
	}
	if ca.Access != "" {
		a.Access = ca.Access
	}
	if ca.Refresh != "" {
		a.Refresh = ca.Refresh
	}
	if ca.Expires > 0 {
		a.Expires = ca.Expires
	}
	return nil
}

func (s *Server) refreshCline(ctx context.Context, a *config.Account) error {
	ca := &cline.Account{Access: a.Access, Refresh: a.Refresh, Expires: a.Expires, Kind: "oauth", Name: a.Name, UID: a.UID}
	if err := cline.RefreshToken(ctx, ca); err != nil {
		return err
	}
	if ca.Access != "" {
		a.Access = ca.Access
	}
	if ca.Refresh != "" {
		a.Refresh = ca.Refresh
	}
	if ca.Expires > 0 {
		a.Expires = ca.Expires
	}
	return nil
}

func (s *Server) refreshMiniMax(ctx context.Context, a *config.Account) error {
	ma := &minimax.Account{Access: a.Access, Refresh: a.Refresh, Expires: a.Expires, UserID: a.UID, Name: a.Name}
	if err := minimax.RefreshToken(ctx, ma); err != nil {
		return err
	}
	if ma.Access != "" {
		a.Access = ma.Access
	}
	if ma.Refresh != "" {
		a.Refresh = ma.Refresh
	}
	if ma.Expires > 0 {
		a.Expires = ma.Expires
	}
	return nil
}

// refreshWorkBuddy：这家的 token 没有公开的刷新接口，
// 只有登录时换一次。所谓「刷新」实际上是重跑 ExchangeToken，
// 但需要 RefreshToken 与设备身份，配置里目前没有存设备私钥，
// 所以这里只做「到期前提醒」，不做自动刷新。
//
// 到期后的正确做法：让用户在面板重新登录一次。
func (s *Server) refreshWorkBuddy(ctx context.Context, a *config.Account) error {
	// 标记为需要重新登录，面板会显示醒目提示
	if a.Expires > 0 && a.Expires < time.Now().UnixMilli() {
		a.CheckinOutcome = "token_expired"
	}
	return errSkipRefresh
}
