package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WorkBuddy token 续期。
//
// 与常见的 OAuth 不同，这边的刷新凭证放在**请求头**里，不在 body：
//
//	POST {origin}/v2/plugin/auth/token/refresh
//	X-Refresh-Token: <refresh>
//	X-Auth-Refresh-Source: plugin
//	X-Domain: <账号所在域>
//	body: {}
//
// 之前网关完全没有实现续期，只存了 access token，所以 token 一到期或被作废，
// 账号就只能重新登录 —— 这也是「很多账号失效」的真正原因。

// PathRefreshToken 是续期端点（相对 origin）。
const PathRefreshToken = "/v2/plugin/auth/token/refresh"

// RefreshResult 是一次续期的结果。
type RefreshResult struct {
	Access            string
	Refresh           string // 上游可能轮换，空表示沿用旧的
	Expires           int64  // access 到期毫秒
	RefreshExpiresAt  int64  // refresh 自身到期毫秒（0 表示未知）
}

// RefreshAccount 用 refresh token 换一对新凭据。
//
// 调用方须保证 acc.Refresh 非空；刷新成功后要把结果写回配置。
func RefreshAccount(ctx context.Context, provider, access, refresh, domain string) (*RefreshResult, error) {
	refresh = strings.TrimSpace(refresh)
	if refresh == "" {
		return nil, fmt.Errorf("workbuddy: 没有 refresh token，无法续期（需要重新登录）")
	}
	origin := originOf(provider)
	if strings.TrimSpace(domain) != "" {
		d := strings.TrimSpace(domain)
		if !strings.HasPrefix(d, "http") {
			d = "https://" + d
		}
		origin = strings.TrimRight(d, "/")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		origin+PathRefreshToken, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Refresh-Token", refresh)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	if strings.TrimSpace(domain) != "" {
		req.Header.Set("X-Domain", strings.TrimSpace(domain))
	}

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: 续期请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("workbuddy: refresh token 已失效（HTTP %d），需要重新登录", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("workbuddy: 续期失败 HTTP %d: %s", resp.StatusCode, snippetOf(raw))
	}

	newAccess := extractAccess(raw, nil)
	if newAccess == "" {
		return nil, fmt.Errorf("workbuddy: 续期响应里没有 access token: %s", snippetOf(raw))
	}
	out := &RefreshResult{
		Access:           newAccess,
		Refresh:          extractRefresh(raw),
		Expires:          jwtExpiryMillis(newAccess),
		RefreshExpiresAt: extractRefreshExpires(raw),
	}
	if out.Expires == 0 {
		// 上游若单独给了 expires，也接住
		var env struct {
			Data struct {
				ExpiresAt int64 `json:"expiresAt"`
				ExpiresIn int64 `json:"expiresIn"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &env) == nil {
			if env.Data.ExpiresAt > 0 {
				out.Expires = env.Data.ExpiresAt
			} else if env.Data.ExpiresIn > 0 {
				out.Expires = nowMillis() + env.Data.ExpiresIn*1000
			}
		}
	}
	return out, nil
}

func snippetOf(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func nowMillis() int64 { return time.Now().UnixMilli() }
