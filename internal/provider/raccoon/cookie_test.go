package raccoon

import "testing"

// 真实 cookie 片段（截断签名部分）
const realCookie = `registration_source=desktop_app;website-language=zh;` +
	`xhx_token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzaWQiOiJzZXMtMDFhMWY2YjktNGQ1Yi03YWRjLTk2ZWYtMDVjYmZmZGEzYWI3Iiwic3ViIjoicHUtMDFhMWY2YjktNGQ0Ny03ZGJkLWE2NWUtM2NhMjE0MjgyYjY2In0.sig;` +
	`user_id=8078413;user_name=RaccoonCharlotte;website-session-present=1`

func TestExtractTokenFromCookie(t *testing.T) {
	got := ExtractTokenFromCookie(realCookie)
	if got == "" {
		t.Fatal("应从 cookie 里提取到 token")
	}
	if !looksLikeJWT(got) {
		t.Fatalf("提取结果不像 JWT: %s", got)
	}
}

// 直接贴纯 token 也要能认
func TestExtractTokenPureJWT(t *testing.T) {
	tok := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig"
	if got := ExtractTokenFromCookie(tok); got != tok {
		t.Fatalf("纯 token 应原样返回，实际 %q", got)
	}
}

// 空串与垃圾串返回空
func TestExtractTokenInvalid(t *testing.T) {
	if got := ExtractTokenFromCookie(""); got != "" {
		t.Errorf("空串应为空，实际 %q", got)
	}
	if got := ExtractTokenFromCookie("foo=bar;baz=qux"); got != "" {
		t.Errorf("无 token 时应为空，实际 %q", got)
	}
}

// 整段 cookie 直接喂给 AccountFromToken
func TestAccountFromCookie(t *testing.T) {
	acc, err := AccountFromToken(realCookie)
	if err != nil {
		t.Fatalf("应能从 cookie 建账号: %v", err)
	}
	if acc.Access == "" {
		t.Fatal("access 不应为空")
	}
	if acc.UID == "" {
		t.Logf("提示：UID 为空（JWT 无 sub），不影响使用")
	}
	t.Logf("uid=%s name=%s", acc.UID, acc.Name)
}
