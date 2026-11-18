package gateway

import (
	"encoding/json"
	"testing"
)

func TestRecordAccountUsage(t *testing.T) {
	resetAccountUsage()
	recordAccountUsage("workbuddy", "acc1", true, usageTokens{In: 100, Out: 10, Credit: 0.02})
	recordAccountUsage("workbuddy", "acc1", true, usageTokens{In: 200, Out: 20, Credit: 0.03})
	recordAccountUsage("workbuddy", "acc2", false, usageTokens{})

	rows := accountUsageRows()
	if len(rows) != 2 {
		t.Fatalf("应有 2 个账号，实际 %d", len(rows))
	}
	// 按积分降序，acc1 在前
	if rows[0].AccountID != "acc1" {
		t.Fatalf("应按积分排序，acc1 应在首位，实际 %s", rows[0].AccountID)
	}
	if rows[0].Requests != 2 || rows[0].InTokens != 300 || rows[0].OutTokens != 30 {
		t.Fatalf("累计值不对: %+v", rows[0])
	}
	if rows[0].Credit != 0.05 {
		t.Fatalf("积分应为 0.05，实际 %v", rows[0].Credit)
	}
	if rows[1].Errors != 1 {
		t.Fatalf("acc2 应有 1 次错误")
	}
}

// 上游响应里的 credit 必须被抠出来
func TestUsageFromJSONCredit(t *testing.T) {
	raw := []byte(`{"id":"x","usage":{"prompt_tokens":864,"completion_tokens":5,"total_tokens":869,"credit":0.02}}`)
	u := usageFromJSON(raw)
	if u.Credit != 0.02 {
		t.Fatalf("应解析出 credit=0.02，实际 %v", u.Credit)
	}
	if u.In != 864 || u.Out != 5 {
		t.Fatalf("token 解析错误: %+v", u)
	}
}

// 流式帧里的 credit 同样要认
func TestUsageFromEventCredit(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"credit":0.5}}`)
	u := usageFromEvent(data)
	if u.Credit != 0.5 {
		t.Fatalf("流式应解析出 credit=0.5，实际 %v", u.Credit)
	}
}

// 不给 credit 的响应不能崩，也不该记成有消耗
func TestUsageWithoutCredit(t *testing.T) {
	raw := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	u := usageFromJSON(raw)
	if u.Credit != 0 {
		t.Fatalf("无 credit 时应为 0，实际 %v", u.Credit)
	}
}

func TestResetAccountUsage(t *testing.T) {
	recordAccountUsage("wb", "a", true, usageTokens{Credit: 1})
	resetAccountUsage()
	if rows := accountUsageRows(); len(rows) != 0 {
		t.Fatalf("清零后应为空，实际 %d 行", len(rows))
	}
}

func TestAccountUsageJSON(t *testing.T) {
	resetAccountUsage()
	recordAccountUsage("wb", "a", true, usageTokens{Credit: 1.234})
	b, _ := json.Marshal(accountUsageRows())
	if len(b) == 0 {
		t.Fatal("应能序列化")
	}
}
