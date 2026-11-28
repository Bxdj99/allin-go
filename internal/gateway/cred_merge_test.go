package gateway

import (
	"path/filepath"
	"testing"

	"allin/internal/config"
)

// 保活只该改凭据，不能覆盖保活期间用户改的其它配置。
//
// 场景：保活跑几分钟（节流 600ms × 账号数），期间用户在面板改了保底。
// 旧实现最后 Save(整份旧快照)，会把保底改动覆盖回旧值 —— 用户改了个寂寞。
// 现在改成只 merge access/refresh/expires。

func newStoreWithTwoAccounts(t *testing.T) (*config.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	st, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := st.Snapshot()
	enabled := true
	cfg.Providers = []*config.Provider{{
		ID: "workbuddy", Name: "WorkBuddy", Enabled: &enabled, Kind: "workbuddy",
		Accounts: []*config.Account{
			{ID: "a1", Type: "token", Access: "old-a1", Refresh: "r1", Enabled: &enabled, Expires: 111},
			{ID: "a2", Type: "token", Access: "old-a2", Refresh: "r2", Enabled: &enabled, Expires: 222},
		},
	}}
	if err := st.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return st, path
}

func readAccount(t *testing.T, st *config.Store, id string) *config.Account {
	t.Helper()
	cfg := st.Snapshot()
	p := cfg.FindProvider("workbuddy")
	if p == nil {
		t.Fatal("找不到 workbuddy")
	}
	for _, a := range p.Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("找不到账号 %s", id)
	return nil
}

// 只合并凭据，保留期间发生的其它配置改动。
func TestMergeCredentialsKeepsUserChanges(t *testing.T) {
	st, _ := newStoreWithTwoAccounts(t)
	s := &Server{store: st}

	// 模拟：保活开始（此时读到旧快照）
	updates := []credUpdate{
		{provider: "workbuddy", account: "a1", access: "new-a1", refresh: "r1b", expires: 999},
	}

	// 模拟：保活期间用户改了保底值
	cfg := st.Snapshot()
	v := 77.0
	cfg.Server.SwitchPolicy.ReserveAmount = &v
	if err := st.Save(cfg); err != nil {
		t.Fatalf("保存用户改动: %v", err)
	}

	// 保活结束，合并凭据
	if err := s.mergeCredentials(updates); err != nil {
		t.Fatalf("mergeCredentials: %v", err)
	}

	// 凭据应当更新
	a1 := readAccount(t, st, "a1")
	if a1.Access != "new-a1" || a1.Expires != 999 {
		t.Errorf("凭据未更新：access=%q expires=%d", a1.Access, a1.Expires)
	}
	// 用户的保底改动必须还在
	got := st.Snapshot().Server.SwitchPolicyOf().ReserveAmountValue()
	if got != 77 {
		t.Errorf("用户的保底改动被覆盖了：期望 77，实际 %v", got)
	}
	// 没被更新的账号保持原样
	a2 := readAccount(t, st, "a2")
	if a2.Access != "old-a2" {
		t.Errorf("a2 不该被改，实际 %q", a2.Access)
	}
}

// 空 updates 不该触发落盘。
func TestMergeCredentialsNoop(t *testing.T) {
	st, _ := newStoreWithTwoAccounts(t)
	s := &Server{store: st}

	before := readAccount(t, st, "a1")
	if err := s.mergeCredentials(nil); err != nil {
		t.Fatalf("空 updates 不该报错: %v", err)
	}
	after := readAccount(t, st, "a1")
	if before.Access != after.Access {
		t.Error("空 updates 不该改动任何凭据")
	}
}

// 账号已不存在时跳过，不 panic、不中断其它更新。
func TestMergeCredentialsMissingAccount(t *testing.T) {
	st, _ := newStoreWithTwoAccounts(t)
	s := &Server{store: st}

	updates := []credUpdate{
		{provider: "workbuddy", account: "不存在的号", access: "x", expires: 1},
		{provider: "workbuddy", account: "a2", access: "new-a2", expires: 888},
	}
	if err := s.mergeCredentials(updates); err != nil {
		t.Fatalf("mergeCredentials: %v", err)
	}
	a2 := readAccount(t, st, "a2")
	if a2.Access != "new-a2" {
		t.Errorf("存在的账号应被更新，实际 %q", a2.Access)
	}
}

// 供应商不存在时也不报错。
func TestMergeCredentialsMissingProvider(t *testing.T) {
	st, _ := newStoreWithTwoAccounts(t)
	s := &Server{store: st}
	updates := []credUpdate{{provider: "nope", account: "a1", access: "x"}}
	if err := s.mergeCredentials(updates); err != nil {
		t.Errorf("供应商不存在不该报错: %v", err)
	}
}
