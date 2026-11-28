package gateway

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"allin/internal/config"
	"allin/internal/provider/trae"
)

// refreshTrae 用 refresh token + 设备私钥给 trae 换新的 Cloud-IDE-JWT。
//
// 为什么必须做：trae 的 access token 有效期很短（实测约 24 小时），
// 过期后上游统一返回 {code:1001, "not able to authenticate you"}。
// 之前 refreshOne 的 switch 里根本没有 KindTrae 分支，
// 于是 trae 账号一旦过期就永远 401，面板还显示「健康」——
// 用户看到的现象就是「能拉起模型、对话出不来」。
//
// trae 的续期不是普通 OAuth：请求要带设备身份的 ECDSA 签名（Proof），
// 所以必须用配置里存的 deviceId / machineId / privateKey，
// 不能只拿 refresh 就发请求。
func (s *Server) refreshTrae(ctx context.Context, a *config.Account) error {
	if a == nil {
		return errSkipRefresh
	}
	refresh := strings.TrimSpace(a.Refresh)
	if refresh == "" {
		return fmt.Errorf("trae: 没有 refresh token，需要重新登录")
	}
	if strings.TrimSpace(a.PrivateKey) == "" || strings.TrimSpace(a.DeviceID) == "" {
		return fmt.Errorf("trae: 缺少设备身份（deviceId/privateKey），需要重新登录")
	}

	ident, err := trae.LoadIdentity(a.DeviceID, a.MachineID, a.PublicKey, a.PrivateKey)
	if err != nil {
		return fmt.Errorf("trae: 设备身份不可用: %w", err)
	}

	// ExchangeToken 内部会依次尝试多个 ClientID，这里整体加个上限
	type result struct {
		acc *trae.Account
		err error
	}
	ch := make(chan result, 1)
	go func() {
		acc, err := trae.ExchangeToken(ident, refresh)
		ch <- result{acc, err}
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("trae: 续期超时")
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("trae: 续期失败: %w", r.err)
		}
		if r.acc == nil || strings.TrimSpace(r.acc.Token) == "" {
			return fmt.Errorf("trae: 续期返回空 token")
		}

		oldAccess := a.Access
		a.Access = r.acc.Token
		// 上游可能轮换 refresh：给了新的就用新的，没给就保留原来的
		if strings.TrimSpace(r.acc.Refresh) != "" {
			a.Refresh = r.acc.Refresh
		}
		if r.acc.Expires > 0 {
			a.Expires = r.acc.Expires
		}
		log.Printf("Trae 续期成功: %s（access 变化=%v）", a.ID, oldAccess != a.Access)
		return nil
	}
}

// traeTokenExpiringSoon 判断 trae 的 token 是不是该续了。
//
// 单独抽出来是因为 trae 的寿命比别家短得多（约 24 小时），
// 通用的 tokenRefreshLead（30 分钟）来不及覆盖一次长时间挂机。
// 这里用「快到期就续」的判定，让后台巡检能提前一天把它换掉。
func traeTokenExpiringSoon(a *config.Account, now time.Time, lead time.Duration) bool {
	if a == nil {
		return false
	}
	if a.Expires <= 0 {
		// 老配置可能没存过期时间，保守起见按「该续」处理
		return true
	}
	return a.Expires <= now.Add(lead).UnixMilli()
}
