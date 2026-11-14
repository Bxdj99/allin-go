// 本文件是文心一言网页版的「聊天接线位」。
//
// 结论先说清楚：这一家**只能接到认证层**，聊天本体在本轮明确回 501，不硬写。
// 理由是同目录 wenxin.go 里已经交代过的那三条缺口（请求体结构、防爬头/签名、
// 登录态从 WebView 转成可复用凭据），一条都没有可取证依据：
//
//  1. POST https://chat.baidu.com/aichat/api/conversation 的请求体长什么样
//     （conversationId / chatId 从哪来、message 数组每项要哪些字段）—— 源码里没有。
//  2. 该接口的防爬头与签名（百度网页版通常要 STOKEN / BAIDUID，还可能带 acs-token、
//     csrf 之类）—— 源码里一处都没提。
//  3. 上游的响应形状（是 SSE 还是分段 JSON、正文在哪个字段）—— 无从判断。
//
// 凭空写一个「看起来能跑」的实现，只会把错误从「明确没接」变成线上 403 / 加密失败，
// 更难查。所以这里把边界做干净：账号、Cookie 拆解、登录入口、网关分派、错误到 HTTP
// 状态的映射都已经接好，真机抓包确认后**只需要补 Run 这一个函数**。
package wenxin

import (
	"context"
	"errors"
	"io"
	"strings"
)

// ErrNoCredential 表示账号里没有 Cookie 登录态（配置里 access 就是整条 Cookie 串）。
var ErrNoCredential = errors.New("wenxin: 账号缺少 Cookie 登录态")

// Run 是文心网页版的聊天入口。
//
// 当前行为：
//   - 账号没有 Cookie → ErrNoCredential（网关映射成 401，可换号重试）
//   - 其余情况        → ErrNotImplemented（网关映射成 501，并在响应里带出原因）
//
// 补实现时需要做的事（拿到真实抓包后）：
//  1. 用 oai.ParseChat 把请求体压成纯文本消息（与 CatPaw / Antigravity 同一口径）；
//  2. 先建会话拿 conversationId，再按百度网页版的字段发消息；
//  3. 响应按 SSE 逐帧解，正文用 oai.Accumulator 取增量，再经 oai.NewStream 回吐；
//     非流式则用 oai.Completion 聚合成一个 chat.completion。
func Run(ctx context.Context, acc *Account, model string, reqBody []byte, out io.Writer, stream bool) error {
	_ = ctx
	_ = model
	_ = reqBody
	_ = out
	_ = stream
	if acc == nil || strings.TrimSpace(acc.Cookie) == "" {
		return ErrNoCredential
	}
	return ErrNotImplemented
}
