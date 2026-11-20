package raccoon

import "testing"

func TestAccountFromToken(t *testing.T) {
	acc, err := AccountFromToken("  eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig  ")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Access != "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig" {
		t.Fatalf("应去掉首尾空白，实际 %q", acc.Access)
	}
	if !acc.IsDesktop() {
		t.Fatal("cookie 形态应标记为 desktop（无续期能力）")
	}
	if acc.ID == "" {
		t.Fatal("应生成账号 ID")
	}
}

func TestAccountFromTokenStripsBearer(t *testing.T) {
	acc, err := AccountFromToken("Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Access != "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig" {
		t.Fatalf("应剥掉 Bearer 前缀，实际 %q", acc.Access)
	}
}

func TestAccountFromTokenEmpty(t *testing.T) {
	if _, err := AccountFromToken("   "); err == nil {
		t.Fatal("空 token 应报错")
	}
}

// cookie 账号不能假装能续期
func TestDesktopAccountCannotRefresh(t *testing.T) {
	acc, _ := AccountFromToken("eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig")
	if err := RefreshToken(nil, acc); err == nil {
		t.Fatal("desktop 账号续期应被拒绝")
	}
}
