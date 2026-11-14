// Package wenxin 是「文心一言网页版」（chat.baidu.com）的最小占位实现。
//
// 诚实交代：All-in 里这家**只有 WebView 登录壳，没有任何请求体构造、没有签名、
// 也没有聊天调用**。全部可取证的事实只有三处：
//
//	app/src/main/assets/default_config.json
//	    "id": "wenxin_web"
//	    "baseUrl": "https://chat.baidu.com/aichat/api/conversation"
//	    "loginUrl": "https://wappass.baidu.com/passport/?login&u=https%3A%2F%2Fwenxin.baidu.com%2F"
//	    "protocol": "openai"
//	    "note": "只有 WebView 登录壳，没有请求体构造与签名；packages 里也没有这一家"
//	LoginActivity.routeLogin()
//	    webView.loadUrl("https://wappass.baidu.com/passport/?login&u=https%3A%2F%2Fwenxin.baidu.com%2F");
//	LoginActivity.getProviderDisplayName()
//	    "wenxin_web" → "百度文心"
//
// 所以本包只导出「确定存在的常量 + 账号结构 + 登录入口」，其余一律返回
// ErrNotImplemented，绝不臆造签名 / 头 / 请求体。
//
// 真实的实现缺口（要补的话必须抓包确认，不能猜）：
//
//  1. chat.baidu.com/aichat/api/conversation 的请求体结构（对话 id、message 数组、
//     是否要 ERNIE 模型 id）—— 源码里没有。
//  2. 该接口的防爬头 / 签名（百度网页版一般要 STOKEN、BAIDUID、还可能带
//     acs-token / csrf 之类），源码里没有一处提到。
//  3. 登录态怎么从 WebView 转成可复用的凭据：CatPaw 那段是 CookieManager 扫描
//     （LoginActivity 约 1492 行起），文心没有对应代码。
package wenxin

import (
	"context"
	"errors"
	"strings"
)

// 来源可查的常量（全部直接抄自 default_config.json 与 LoginActivity）。
const (
	// ProviderID 是供应商 id。
	ProviderID = "wenxin_web"

	// Name 是显示名（LoginActivity 里 "百度文心"）。
	Name = "百度文心"

	// Origin 网页版根域。
	Origin = "https://chat.baidu.com"
	// ChatURL 配置里声明的对话端点（**裸值，未验证**）。
	ChatURL = "https://chat.baidu.com/aichat/api/conversation"
	// HomeURL 文心一言首页。
	HomeURL = "https://wenxin.baidu.com/"

	// PassportURL 百度统一登录页。
	PassportURL = "https://wappass.baidu.com/passport/"
	// LoginURL 是 App 里原样 loadUrl 的那条登录地址（已带 u= 回跳）。
	LoginURL = "https://wappass.baidu.com/passport/?login&u=https%3A%2F%2Fwenxin.baidu.com%2F"
)

// Models 是 default_config.json 里声明的模型名。
//
// 注意：这些是配置里的「对外模型名」，不是百度上游真实模型 id（真 id 得抓包确认）。
var Models = []string{"ernie-4.0-turbo", "ernie-speed", "ernie-3.5"}

// ErrNotImplemented 表示该能力在 Java 侧根本不存在实现，本包不会假装有。
var ErrNotImplemented = errors.New("wenxin: 该能力在 All-in 源码中没有实现（只有 WebView 登录壳），需要抓包确认后才能补")

// Account 是一个文心一言网页版账号。
//
// 因为源码没定义账号结构，这里的字段是「网页登录态通常长这样」的最小集合，
// 全部可以留空；Cookie 是唯一必需的（登录态整体就是浏览器 Cookie）。
type Account struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	UID     string `json:"uid,omitempty"`
	Cookie  string `json:"cookie,omitempty"`  // 完整 Cookie 串（登录态本体）
	BDUSS   string `json:"bduss,omitempty"`   // 百度账号主票据
	STOKEN  string `json:"stoken,omitempty"`  // 百度网页版接口票据
	BDUID   string `json:"bduid,omitempty"`   // BAIDUID
	AddedAt int64  `json:"addedAt,omitempty"` // 入池时间（毫秒）
	Enabled bool   `json:"enabled,omitempty"`
}

// Headers 返回聊天请求头。
//
// Cookie 是确定的（登录态就是它）；User-Agent / Referer / Origin 是浏览器网页的
// 常规形态，属于**按网页版惯例给出的骨架**，源码里没有依据，接线上请以抓包为准。
func Headers(acc *Account) map[string]string {
	cookie := ""
	if acc != nil {
		cookie = acc.Cookie
	}
	return map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json, text/plain, */*",
		"Cookie":       cookie,
		"Origin":       Origin,
		"Referer":      Origin + "/",
		// 百度网页版对非浏览器 UA 很敏感，这里给一个通用 Chrome UA 占位。
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	}
}

// FromCookie 用一条浏览器 Cookie 串建账号，并顺手把 BDUSS / STOKEN / BAIDUID 拆出来。
//
// 之所以拆：文心网页版的接口通常直接用 STOKEN 之类，单独留着方便后续实现。
func FromCookie(raw string) *Account {
	acc := &Account{Cookie: strings.TrimSpace(raw), Enabled: true}
	for _, part := range strings.Split(acc.Cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "BDUSS":
			acc.BDUSS = kv[1]
		case "STOKEN":
			acc.STOKEN = kv[1]
		case "BAIDUID":
			acc.BDUID = kv[1]
		}
	}
	return acc
}

// Flow 是一次登录。文心走的是 WebView 打开百度通行证，登录完由壳自己抓 Cookie，
// 所以这里只有一条 URL，没有 state / sid / PKCE。
type Flow struct {
	URL string
}

// StartLogin 返回需要打开的登录地址（就是 App 里 loadUrl 的那一条）。
//
// 登录完成后没有「服务端轮询」可用：源码里没有 wenxin 的 poll 通道。
func StartLogin() (*Flow, error) {
	return &Flow{URL: LoginURL}, nil
}

// PollLogin 恒返回 ErrNotImplemented —— 源码里没有文心的凭据轮询通道。
//
// 可用的替代路径：在 WebView 里登录后从 CookieManager 读 https://chat.baidu.com 的
// Cookie，再用 FromCookie 建账号（CatPaw 那段扫描逻辑可参考 LoginActivity）。
func PollLogin(ctx context.Context, f *Flow) (*Account, error) {
	_ = ctx
	_ = f
	return nil, ErrNotImplemented
}

// RefreshToken 未实现：网页登录态靠 Cookie，没有 refresh_token 概念，
// 过期只能重开 WebView 再登一次（源码里也没有任何刷新代码）。
func RefreshToken(ctx context.Context, acc *Account) error {
	_ = ctx
	_ = acc
	return ErrNotImplemented
}

// Chat 未实现：对话端点已知（ChatURL），但请求体、必填头、签名全部无从取证，
// 凭空写一个「看起来能跑」的实现只会把错误变成线上 403/加密失败。
func Chat(ctx context.Context, acc *Account, model string, messages []string) error {
	_ = ctx
	_ = acc
	_ = model
	_ = messages
	return ErrNotImplemented
}
