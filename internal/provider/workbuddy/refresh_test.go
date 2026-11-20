package workbuddy

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func mkJWT(exp int64) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	p, _ := json.Marshal(map[string]any{"exp": exp, "sub": "u1"})
	pl := base64.RawURLEncoding.EncodeToString(p)
	return h + "." + pl + ".sig"
}

// 之前只提取 access，refresh 被丢弃 —— 这是账号无法续期的根因
func TestExtractRefresh(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"accessToken":"a.b.c","refreshToken":"REFRESH-1"}}`)
	if got := extractRefresh(raw); got != "REFRESH-1" {
		t.Fatalf("应提取到 REFRESH-1，实际 %q", got)
	}
}

func TestExtractRefreshAltField(t *testing.T) {
	raw := []byte(`{"data":{"refresh_token":"R2"}}`)
	if got := extractRefresh(raw); got != "R2" {
		t.Fatalf("应认 refresh_token，实际 %q", got)
	}
}

func TestExtractRefreshEmpty(t *testing.T) {
	if got := extractRefresh([]byte(`{"data":{"accessToken":"a.b.c"}}`)); got != "" {
		t.Fatalf("没有 refresh 时应返回空，实际 %q", got)
	}
}

// expires 应从 JWT 的 exp 解出来
func TestJWTExpiry(t *testing.T) {
	tok := mkJWT(1796997626)
	got := jwtExpiryMillis(tok)
	if got != 1796997626000 {
		t.Fatalf("应解出 1796997626000，实际 %d", got)
	}
}

func TestJWTExpiryInvalid(t *testing.T) {
	if got := jwtExpiryMillis("not-a-jwt"); got != 0 {
		t.Fatalf("非法 token 应返回 0，实际 %d", got)
	}
}

// 没有 refresh 时，续期要给出明确错误而不是静默失败
func TestRefreshWithoutToken(t *testing.T) {
	_, err := RefreshAccount(nil, "workbuddy", "a.b.c", "", "")
	if err == nil {
		t.Fatal("没有 refresh 应报错")
	}
}
