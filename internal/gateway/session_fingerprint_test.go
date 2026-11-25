package gateway

import (
	"encoding/json"
	"testing"
)

// 会话指纹同时用作 prompt_cache_key 的会话段。
// 它在同一条对话的每一轮必须保持不变，否则上游前缀缓存全部失效，
// 每轮按全量 prompt 计费 —— 线上实测输入 6.7 亿 token / 输出 67 万。

func body(t *testing.T, msgs ...map[string]any) []byte {
	t.Helper()
	out, err := json.Marshal(map[string]any{"messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func user(text string) map[string]any { return map[string]any{"role": "user", "content": text} }
func asst(text string) map[string]any { return map[string]any{"role": "assistant", "content": text} }

// 同一段对话追加消息，指纹必须不变。
func TestFingerprintStableAcrossTurns(t *testing.T) {
	first := "帮我看看这个网关为什么积分消耗这么快"
	round1 := body(t, user(first))
	round2 := body(t, user(first), asst("我看一下"), user("具体是哪里"))
	round3 := body(t, user(first), asst("我看一下"), user("具体是哪里"), asst("缓存问题"), user("怎么修"))

	f1, f2, f3 := sessionFingerprint(round1), sessionFingerprint(round2), sessionFingerprint(round3)
	if f1 == "" {
		t.Fatal("指纹不应为空")
	}
	if f1 != f2 || f2 != f3 {
		t.Errorf("同一对话换轮后指纹应稳定：%s / %s / %s", f1, f2, f3)
	}
}

// 不同对话必须得到不同指纹。
func TestFingerprintDiffersAcrossSessions(t *testing.T) {
	a := sessionFingerprint(body(t, user("第一个话题：签到为什么没跑")))
	b := sessionFingerprint(body(t, user("第二个话题：额度口径怎么算")))
	if a == b {
		t.Error("不同对话不应得到相同指纹")
	}
}

// 客户端自带的会话标识优先，且跨轮稳定。
func TestFingerprintPrefersClientSessionID(t *testing.T) {
	mk := func(extra map[string]any, msgs ...map[string]any) []byte {
		m := map[string]any{"messages": msgs}
		for k, v := range extra {
			m[k] = v
		}
		out, _ := json.Marshal(m)
		return out
	}

	// 两轮的第一条 user 消息不同，但 session_id 相同 → 指纹应相同
	b1 := mk(map[string]any{"session_id": "sess-abc"}, user("第一轮问法 A"))
	b2 := mk(map[string]any{"session_id": "sess-abc"}, user("第一轮问法 B（完全不同）"), asst("答"), user("继续"))

	if sessionFingerprint(b1) != sessionFingerprint(b2) {
		t.Error("相同 session_id 应得到相同指纹")
	}
}

// agent 客户端的注入式首条消息不应影响指纹稳定性。
func TestFingerprintSkipsInjectedContext(t *testing.T) {
	real := "把 gateway.log 里的 429 统计出来"

	// 第一轮：注入内容在前，真实提问在后
	b1 := body(t,
		user("<system-reminder>current working directory: /workspace/allin-go</system-reminder>"),
		user(real),
	)
	// 第二轮：注入内容变了（用户换了目录），真实提问不变
	b2 := body(t,
		user("<system-reminder>current working directory: /tmp</system-reminder>"),
		user(real),
		asst("我看看"),
		user("继续"),
	)

	if sessionFingerprint(b1) != sessionFingerprint(b2) {
		t.Error("注入内容变化不应导致会话指纹变化（否则缓存 key 每轮都变）")
	}
}

// 纯注入、没有真实提问时，仍要给出可用指纹（不能为空）。
func TestFingerprintFallsBackWhenOnlyInjected(t *testing.T) {
	b := body(t, user("<system-reminder>cwd: /a</system-reminder>"))
	if sessionFingerprint(b) == "" {
		t.Error("只有注入内容时也应给出指纹，不能为空")
	}
}

// 空消息列表返回空指纹（表示不做绑定）。
func TestFingerprintEmptyMessages(t *testing.T) {
	if got := sessionFingerprint([]byte(`{"messages":[]}`)); got != "" {
		t.Errorf("空消息应返回空指纹，得到 %q", got)
	}
}
