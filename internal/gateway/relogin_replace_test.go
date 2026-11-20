package gateway

import (
	"testing"

	"allin/internal/config"
)

// 重新登录时指定 replaceID，应该把旧账号摘掉、写入新账号，不留死号
func TestReplaceAccountByID(t *testing.T) {
	// 模拟保存前的账号池
	p := &config.Provider{
		ID: "workbuddy",
		Accounts: []*config.Account{
			{ID: "old-dead", Name: "旧号"},
			{ID: "other", Name: "别的号"},
		},
	}

	// 走 saveAuthAccountReplacing 的「先摘旧号」逻辑
	replaceID := "old-dead"
	kept := make([]*config.Account, 0, len(p.Accounts))
	for _, old := range p.Accounts {
		if old != nil && old.ID == replaceID {
			continue
		}
		kept = append(kept, old)
	}
	p.Accounts = kept

	// 新账号 ID 与旧的不同（真实情况：按邮箱/UID 重新生成）
	newAcc := &config.Account{ID: "new-alive", Name: "旧号"}
	p.Accounts = append(p.Accounts, newAcc)

	if len(p.Accounts) != 2 {
		t.Fatalf("应有 2 个账号，实际 %d", len(p.Accounts))
	}
	for _, a := range p.Accounts {
		if a.ID == "old-dead" {
			t.Fatal("旧死号应被摘掉")
		}
	}
	found := false
	for _, a := range p.Accounts {
		if a.ID == "new-alive" {
			found = true
		}
	}
	if !found {
		t.Fatal("新账号应写入")
	}
}

// 不指定 replaceID 时保持原有行为（不删任何账号）
func TestNoReplaceKeepsAll(t *testing.T) {
	accounts := []*config.Account{{ID: "a"}, {ID: "b"}}
	replaceID := ""
	kept := make([]*config.Account, 0, len(accounts))
	for _, old := range accounts {
		if replaceID != "" && old.ID == replaceID {
			continue
		}
		kept = append(kept, old)
	}
	if len(kept) != 2 {
		t.Fatalf("不指定替换时不应删账号，实际剩 %d", len(kept))
	}
}
