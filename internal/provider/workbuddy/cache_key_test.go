package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInjectPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := InjectPromptCacheKey(body, "uid-abcdefgh1234", "sess-1")

	var m map[string]any
	json.Unmarshal(out, &m)
	k, _ := m["prompt_cache_key"].(string)
	if k == "" {
		t.Fatal("应注入 prompt_cache_key")
	}
	if !strings.Contains(k, "uid-abcd") {
		t.Fatalf("key 应含账号 uid 前 8 位做隔离，实际 %s", k)
	}
	t.Logf("生成的 key: %s", k)
}

// 同账号同会话必须稳定，否则缓存命中不了
func TestCacheKeyStable(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	a := InjectPromptCacheKey(body, "u1", "same")
	b := InjectPromptCacheKey(body, "u1", "same")
	if string(a) != string(b) {
		t.Fatal("同账号同会话的 key 必须稳定")
	}
}

// 不同会话、不同账号必须不同
func TestCacheKeyIsolation(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	get := func(uid, sess string) string {
		var m map[string]any
		json.Unmarshal(InjectPromptCacheKey(body, uid, sess), &m)
		return m["prompt_cache_key"].(string)
	}
	if get("u1", "s1") == get("u1", "s2") {
		t.Error("不同会话应得到不同 key")
	}
	if get("u1", "s1") == get("u2", "s1") {
		t.Error("不同账号必须隔离，否则会命中别人的缓存")
	}
}

// 客户端自带 key 时绝不覆盖
func TestCacheKeyRespectsClient(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"client-own","messages":[]}`)
	out := InjectPromptCacheKey(body, "u1", "s1")
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["prompt_cache_key"] != "client-own" {
		t.Fatal("不应覆盖客户端自带的 key")
	}
}
