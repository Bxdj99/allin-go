package workbuddy

import (
	"encoding/json"
	"testing"
)

func TestCapMaxTokensDownsizes(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100000}`)
	out := CapMaxTokens(body, 4096)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["max_tokens"].(float64) != 4096 {
		t.Fatalf("应降到 4096，实际 %v", m["max_tokens"])
	}
}

// 客户端值小于上限时不动
func TestCapMaxTokensKeepsSmall(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100}`)
	out := CapMaxTokens(body, 4096)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["max_tokens"].(float64) != 100 {
		t.Fatalf("小于上限时不应改，实际 %v", m["max_tokens"])
	}
}

// max_completion_tokens 也要处理
func TestCapMaxTokensAltField(t *testing.T) {
	body := []byte(`{"model":"m","max_completion_tokens":99999}`)
	out := CapMaxTokens(body, 2048)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["max_completion_tokens"].(float64) != 2048 {
		t.Fatalf("应降到 2048，实际 %v", m["max_completion_tokens"])
	}
}

// cap<=0 表示不限制
func TestCapMaxTokensDisabled(t *testing.T) {
	body := []byte(`{"max_tokens":100000}`)
	if string(CapMaxTokens(body, 0)) != string(body) {
		t.Fatal("cap=0 时应原样返回")
	}
}
