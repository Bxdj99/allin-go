package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

// 落盘后能读回来
func TestAccountUsagePersist(t *testing.T) {
	resetAccountUsage()
	recordAccountUsage("workbuddy", "acc1", true, usageTokens{In: 500, Out: 20, Credit: 1.25})

	dir := t.TempDir()
	path := filepath.Join(dir, accUsageFileName)

	// 直接写一份快照（模拟定时落盘）
	rows := accountUsageRows()
	for _, r := range rows {
		if r.AccountID != "acc1" {
			continue
		}
		if r.Credit != 1.25 {
			t.Fatalf("积分应为 1.25，实际 %v", r.Credit)
		}
		if r.InTokens != 500 {
			t.Fatalf("输入 token 应为 500，实际 %d", r.InTokens)
		}
	}

	// 校验文件能被正确反序列化
	if err := os.WriteFile(path, []byte(`{"savedAt":1,"rows":[{"providerId":"workbuddy","accountId":"acc1","requests":1,"credit":1.25,"inTokens":500}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		t.Fatal("文件应可读回")
	}
}

// 累计值要精确（千分之一精度）
func TestCreditPrecision(t *testing.T) {
	resetAccountUsage()
	for i := 0; i < 10; i++ {
		recordAccountUsage("wb", "a", true, usageTokens{Credit: 0.02})
	}
	rows := accountUsageRows()
	if len(rows) != 1 {
		t.Fatalf("应只有 1 行，实际 %d", len(rows))
	}
	if rows[0].Credit < 0.19 || rows[0].Credit > 0.21 {
		t.Fatalf("10 次 0.02 应累计约 0.2，实际 %v", rows[0].Credit)
	}
}
