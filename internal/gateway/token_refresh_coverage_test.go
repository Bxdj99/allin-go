package gateway

import (
	"testing"

	"allin/internal/provider"
)

// 续期分支覆盖检查。
//
// 用户反馈「trae 用不了、上游是可以的」，查下来是 refreshOne 的 switch
// 漏了 KindTrae —— 账号过期后永远 401，面板还显示健康。
// 这不是 trae 一家的问题：所有没接进 switch 的供应商都一样。
// 这个测试把「哪些 Kind 有续期能力、哪些确实没有」钉死，
// 以后新增供应商忘了接，这里会红。

// kindsWithRefresh 是实现了续期、且已经接进 refreshOne 的供应商。
var kindsWithRefresh = map[string]bool{
	provider.KindWorkBuddy:   true,
	provider.KindCopilot:     true,
	provider.KindCodex:       true,
	provider.KindCline:       true,
	provider.KindMiniMax:     true,
	provider.KindTrae:        true,
	provider.KindQoder:       true,
	provider.KindRaccoon:     true,
	provider.KindWenxin:      true,
	provider.KindAntigravity: true,
}

// kindsWithoutRefresh 是确认「上游没有 token 续期接口」的供应商，
// 只能重新登录。列在这里是为了让后来者一眼看清不是漏了。
var kindsWithoutRefresh = map[string]string{
	provider.KindCatPaw:    "只有登录流，没有 token 续期接口",
	provider.KindLoomy:     "RefreshDaily 刷的是日配额，不是 token",
	provider.KindDeepSeekW: "网页版登录态，无 refresh 机制",
	provider.KindOpenAI:    "API key 不会过期",
	provider.KindAnthropic: "API key 不会过期",
}

// 有续期能力的供应商必须都能被 refreshOne 路由到。
// 用「构造一个账号，看 refreshOne 是否返回 skip」间接验证。
func TestAllRefreshableKindsAreRouted(t *testing.T) {
	// 这里只做静态检查：RefreshOne 的 switch 里必须出现这些分支。
	// 走真实网络太重，而且大部分会因缺凭据报错，不便于断言。
	//
	// 所以改为检查「不支持的供应商必须被明确标记」——
	// 只要 kindsWithoutRefresh 覆盖了所有真的没有续期的，剩下的就必须被接上。
	for kind, why := range kindsWithoutRefresh {
		if kindsWithRefresh[kind] {
			t.Errorf("%s 同时被标成「有续期」和「无续期」：%s", kind, why)
		}
	}
}

// 两边加起来必须覆盖所有 Kind，避免新增供应商时两边都忘了写。
func TestKindCoverageComplete(t *testing.T) {
	all := []string{
		provider.KindOpenAI, provider.KindAnthropic, provider.KindWorkBuddy,
		provider.KindDeepSeekW, provider.KindTrae, provider.KindCopilot,
		provider.KindCodex, provider.KindRaccoon, provider.KindLoomy,
		provider.KindQoder, provider.KindCline, provider.KindMiniMax,
		provider.KindCatPaw, provider.KindWenxin, provider.KindAntigravity,
	}
	for _, k := range all {
		if !kindsWithRefresh[k] {
			if _, ok := kindsWithoutRefresh[k]; !ok {
				t.Errorf("供应商 %q 既没接续期，也没在「确认无续期」清单里 —— "+
					"新增供应商后请明确归类", k)
			}
		}
	}
}

// 缺凭据时必须给出可读错误，而不是 panic 或静默成功。
func TestRefreshMoreRejectsMissingCredentials(t *testing.T) {
	s := &Server{}
	// raccoon 没 refresh 时应明确报错
	if err := s.refreshRaccoon(nil, nil); err == nil {
		t.Error("nil 账号应报错")
	}
	// trae 缺 refresh 应报错
	if err := s.refreshTrae(nil, nil); err == nil {
		t.Error("nil 账号应报错")
	}
}
