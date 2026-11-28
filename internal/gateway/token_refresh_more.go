package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"allin/internal/config"
	"allin/internal/provider/antigravity"
	"allin/internal/provider/qoder"
	"allin/internal/provider/raccoon"
	"allin/internal/provider/wenxin"
)

// 补齐续期适配的几家。
//
// 为什么这些人以前「用不了」：
// 各 provider 包里 RefreshToken / NeedsRefresh 都是现成的，
// 但 refreshOne 的 switch 只接了 copilot / codex / cline / minimax / workbuddy，
// 其余几家根本走不到续期分支 —— 账号一过期就永远 401，
// 面板还显示「健康」，用户看到的就是「能拉起模型、对话出不来」。
// trae 是这次反馈暴露出来的一个，结构上所有没接的供应商都一样。

func (s *Server) refreshQoder(ctx context.Context, a *config.Account) error {
	if a == nil {
		return errSkipRefresh
	}
	qa := &qoder.Account{
		ID: a.ID, Name: a.Name, Email: a.Email, UID: a.UID,
		Token:   a.Key(),
		Refresh: a.Refresh,
	}
	if err := qoder.RefreshToken(ctx, qa); err != nil {
		return err
	}
	// 刷新可能同时更新 token 与 device 侧凭据，按非空回写
	if qa.Token != "" {
		a.Access = qa.Token
	}
	if qa.Refresh != "" {
		a.Refresh = qa.Refresh
	}
	// qoder 的 Account 没有 Expires 字段，过期时间由上游返回的 token 自带；
	// 这里按「刚刷新过」记一个保守窗口，避免下次巡检又白刷一遍。
	a.Expires = nowPlusHours(23)
	return nil
}

func (s *Server) refreshRaccoon(ctx context.Context, a *config.Account) error {
	if a == nil {
		return errSkipRefresh
	}
	// 浏览器 cookie 来的账号本来就没有续期能力，明确跳过而不是报错
	if strings.TrimSpace(a.Refresh) == "" {
		return fmt.Errorf("raccoon: 没有 refresh token（cookie 账号需要重新登录）")
	}
	ra := &raccoon.Account{
		ID: a.ID, Name: a.Name, UID: a.UID,
		Access: a.Access, Token: a.Access,
		Refresh: a.Refresh, Expires: a.Expires,
	}
	if err := raccoon.RefreshToken(ctx, ra); err != nil {
		return err
	}
	if ra.Access != "" {
		a.Access = ra.Access
	} else if ra.Token != "" {
		a.Access = ra.Token
	}
	if ra.Refresh != "" {
		a.Refresh = ra.Refresh
	}
	if ra.Expires > 0 {
		a.Expires = ra.Expires
	}
	return nil
}

func (s *Server) refreshWenxin(ctx context.Context, a *config.Account) error {
	if a == nil {
		return errSkipRefresh
	}
	// 文心的登录态是整条 Cookie 串，续期就是拿它去换新票据
	wa := wenxin.FromCookie(a.Key())
	wa.ID, wa.Name, wa.UID = a.ID, a.Name, a.UID
	oldCookie := wa.Cookie
	if err := wenxin.RefreshToken(ctx, wa); err != nil {
		return err
	}
	// 拿到新 Cookie 才回写；没变就别动，免得把好好的登录态覆盖成空
	if wa.Cookie != "" && wa.Cookie != oldCookie {
		a.Access = wa.Cookie
	}
	return nil
}

func (s *Server) refreshAntigravity(ctx context.Context, a *config.Account) error {
	if a == nil {
		return errSkipRefresh
	}
	key := a.Key()
	ga := &antigravity.Account{
		ID: a.ID, Name: a.Name, Email: a.Email,
		Access: key, Token: key,
		Refresh: a.Refresh, Expires: a.Expires,
		Project: a.Domain, Enabled: true,
	}
	if err := antigravity.RefreshToken(ctx, ga); err != nil {
		return err
	}
	if ga.Access != "" {
		a.Access = ga.Access
	} else if ga.Token != "" {
		a.Access = ga.Token
	}
	if ga.Refresh != "" {
		a.Refresh = ga.Refresh
	}
	if ga.Expires > 0 {
		a.Expires = ga.Expires
	}
	return nil
}

// nowPlusHours 返回「现在 + n 小时」的毫秒时间戳。
// 给那些上游不返回明确过期时间的供应商用：记一个保守窗口，
// 既不会立刻被再次续期，也不会被误判成永久有效。
func nowPlusHours(n int) int64 {
	return time.Now().Add(time.Duration(n) * time.Hour).UnixMilli()
}
