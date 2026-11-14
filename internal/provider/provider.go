// Package provider 负责把统一请求翻译成上游各家供应商的请求形态。
//
// 设计原则：能透传就透传。请求体除了改写 model 名之外一律原样送给上游，
// 响应（含 SSE 流）也原样回传。这样上游新增字段、新工具调用格式都不需要跟着改代码，
// 只有鉴权方式、路径和少数必填头需要按 kind 分派。
package provider

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"allin/internal/config"
	"allin/internal/provider/antigravity"
	"allin/internal/provider/catpaw"
	"allin/internal/provider/cline"
	"allin/internal/provider/codex"
	"allin/internal/provider/copilot"
	"allin/internal/provider/deepseekweb"
	"allin/internal/provider/loomy"
	"allin/internal/provider/minimax"
	"allin/internal/provider/qoder"
	"allin/internal/provider/raccoon"
	"allin/internal/provider/trae"
	"allin/internal/provider/workbuddy"
	"allin/internal/provider/wenxin"
)

// Kind 是供应商协议族。
const (
	KindOpenAI    = "openai"    // 标准 OpenAI 兼容（大多数国内厂商都吃这套）
	KindAnthropic = "anthropic" // Anthropic Messages
	KindWorkBuddy = "workbuddy" // 腾讯 WorkBuddy（copilot.tencent.com），头略特殊
	KindDeepSeekW = "deepseek-web"
	KindTrae      = "trae"
	KindCopilot   = "copilot"
	KindCodex     = "codex"
	KindRaccoon   = "raccoon"
	KindLoomy     = "loomy"
	KindQoder     = "qoder"
	KindCline     = "cline"
	KindMiniMax   = "minimax"
	// 下面三家是「网页版私有协议」：请求拼装与响应翻译都在各自包里完成，
	// 不走 BuildRequest 那套透传（详见 gateway.forwardWebChat）。
	KindCatPaw      = "catpaw"      // 美团妙手（ai.catpaw.meituan.com）
	KindWenxin      = "wenxin"      // 百度文心一言网页版（chat.baidu.com）
	KindAntigravity = "antigravity" // Google Antigravity（cloudcode-pa）
)

// NormalizeKind 收敛几个历史别名，免得配置里写 wenxin_web 就认不出来。
//
// 别名只做等价映射，不会把未知 kind 变成已知 kind（那些仍然按原值往下走）。
func NormalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "wenxin_web", "wenxin-web", "wenxinweb":
		return KindWenxin
	default:
		return kind
	}
}

// HTTPClient 是所有上游请求共用的客户端。
var HTTPClient = &http.Client{
	Timeout: 0, // 流式响应不能设总超时，靠 Transport 的连接级超时兜底
	Transport: &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 3 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// caBundle 是内置的 Mozilla CA 根证书集合。
//
// 为什么必须内嵌：静态链接的 Go 二进制在 Android 上会去 /etc/ssl/certs 找根证书，
// 而 Android 把证书放在 /system/etc/security/cacerts，两边对不上，
// 结果就是所有 HTTPS 请求报 x509: certificate signed by unknown authority。
//
//go:embed ca-certificates.crt
var caBundle []byte

func init() {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(caBundle)

	// 所有供应商包共用同一份带内置 CA 的 Transport。
	// 静态二进制在 Android 上找不到系统根证书（证书在 /system/etc/security/cacerts，
	// 而 Go 只认 /etc/ssl/certs），不统一注入就会全员 x509 失败。
	// 连接层加固：参数取自 wb2api 的实测经验（internal/upstream/transport.go）
	//   - 默认 Dialer 没有连接上限，半死 TCP 能让单次 TTFB 卡到 900 秒以上
	//   - 默认 keepalive 2 小时才发首个探测，NAT 黑洞里的死连接会被一直复用
	//   - ResponseHeaderTimeout 管「首字节到达前」，SSE 头到达后的长流不受影响
	dialer := &net.Dialer{
		Timeout:   10 * time.Second, // 连不上就快速失败去换号，别干等
		KeepAlive: 15 * time.Second, // 让死连接在 15~30 秒内被内核掐掉
	}
	sharedTransport := &http.Transport{
		Proxy:       nil,
		DialContext: dialer.DialContext,
		// 空闲连接保留 30s：WAF 风暴后上游常掐闲置连接，90s 池里多半已是死连接
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 3 * time.Second,
		// 聊天 SSE 首字节前的硬上限。观测到慢模型冷启动最坏 16s，取 60s 留余量；
		// 同时把「半死连接卡死」从 2 分钟压到 1 分钟，配合 3 次换号最坏 3 分钟。
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	trae.HTTPClient.Transport = sharedTransport
	copilot.HTTPClient.Transport = sharedTransport
	codex.HTTPClient.Transport = sharedTransport
	deepseekweb.HTTPClient.Transport = sharedTransport
	raccoon.HTTPClient.Transport = sharedTransport
	loomy.HTTPClient.Transport = sharedTransport
	qoder.HTTPClient.Transport = sharedTransport
	cline.HTTPClient.Transport = sharedTransport
	minimax.HTTPClient.Transport = sharedTransport
	catpaw.HTTPClient.Transport = sharedTransport
	workbuddy.HTTPClient.Transport = sharedTransport
	// antigravity 必须禁 HTTP/2（cloudcode-pa 协商到 h2 会被 reset），单独一份
	agTransport := sharedTransport.Clone()
	agTransport.ForceAttemptHTTP2 = false
	antigravity.HTTPClient = &http.Client{Transport: agTransport, Timeout: antigravity.HTTPClient.Timeout}
	if tr, ok := HTTPClient.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		// 妙手 / 文心这两个包各自持有一个裸 &http.Client{}：在 Android 上它只认
		// /etc/ssl/certs，而系统证书在 /system/etc/security/cacerts，会一律报
		// x509: certificate signed by unknown authority。这里把带内置根证书的
		// 客户端交给它们，省得每个包各内嵌一份 CA。
		catpaw.HTTPClient = HTTPClient
		// Antigravity 不能直接用 HTTPClient：它的 Transport 开着 ForceAttemptHTTP2，
		// 而 cloudcode-pa 协商到 h2 就 reset。单独造一份「同样带内置根证书、但只走
		// HTTP/1.1」的 Transport。
		antigravity.HTTPClient = &http.Client{
			Transport: &http.Transport{
				Proxy:                 nil,
				ForceAttemptHTTP2:     false,
				MaxIdleConns:          16,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 3 * time.Second,
				TLSClientConfig:       tr.TLSClientConfig,
			},
		}
	}
}

// Resolve 把 model 名拆成「供应商 + 真实模型名 + 选中的账号」。
//
// 支持的写法：
//   - provider/model       直接指定
//   - group/<组id>         路由组，按 strategy 挑一个目标
//   - 裸模型名             仅在 routing.bareModelFallback 开启时，在所有供应商里找
func Resolve(cfg *config.Config, model string) (*config.Provider, *config.Account, string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, nil, "", fmt.Errorf("model 不能为空")
	}

	if strings.HasPrefix(model, "group/") {
		gid := strings.TrimPrefix(model, "group/")
		g := cfg.FindGroup(gid)
		if g == nil {
			return nil, nil, "", fmt.Errorf("路由组不存在: %s", gid)
		}
		if len(g.Targets) == 0 {
			return nil, nil, "", fmt.Errorf("路由组 %s 没有目标", gid)
		}
		var lastErr error
		for _, target := range g.Targets {
			p, a, m, err := Resolve(cfg, target)
			if err != nil {
				lastErr = err
				continue
			}
			return p, a, m, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("路由组 %s 内所有目标都不可用", gid)
		}
		return nil, nil, "", lastErr
	}

	if pid, name, ok := config.SplitModel(model); ok {
		p := cfg.FindProvider(pid)
		if p == nil {
			return nil, nil, "", fmt.Errorf("供应商不存在: %s", pid)
		}
		if !p.IsEnabled() {
			return nil, nil, "", fmt.Errorf("供应商已停用: %s", pid)
		}
		a := pickAccount(p)
		if a == nil {
			return nil, nil, "", fmt.Errorf("供应商 %s 没有可用账号", pid)
		}
		return p, a, name, nil
	}

	if !cfg.Routing.BareModelFallback {
		return nil, nil, "", fmt.Errorf("模型名需要写成 provider/model 形式（当前未开启裸名兜底）")
	}
	for _, p := range cfg.Providers {
		if p == nil || !p.IsEnabled() {
			continue
		}
		for _, m := range p.Models {
			if m == model {
				if a := pickAccount(p); a != nil {
					return p, a, model, nil
				}
			}
		}
	}
	return nil, nil, "", fmt.Errorf("没有供应商提供模型: %s", model)
}

// pickAccount 取第一个可用账号（P3 会换成按用量/失败率轮转）。
func pickAccount(p *config.Provider) *config.Account {
	for _, a := range p.Accounts {
		if a.IsEnabled() && a.Key() != "" {
			return a
		}
	}
	// 没配 accounts 时允许直接用 provider.apiKey
	if p.APIKey != "" {
		return &config.Account{Access: p.APIKey, Type: "api"}
	}
	return nil
}

// ModelList 列出所有可用模型（provider/model 形式）。
func ModelList(cfg *config.Config) []string {
	out := make([]string, 0, 64)
	for _, p := range cfg.Providers {
		if p == nil || !p.IsEnabled() {
			continue
		}
		for _, m := range p.Models {
			out = append(out, p.ID+"/"+m)
		}
	}
	for _, g := range cfg.Groups {
		if g != nil && len(g.Targets) > 0 {
			out = append(out, "group/"+g.ID)
		}
	}
	return out
}

// BuildRequest 按 kind 构造发往上游的请求。
//
// apiPath 是「相对客户端入口」的路径后缀，例如 "/chat/completions" 或 "/messages"。
func BuildRequest(kind, baseURL, apiPath, key string, acct *config.Account, body []byte, extra map[string]string) (*http.Request, error) {
	kind = NormalizeKind(kind)
	// 小浣熊的聊天端点有自己的一段前缀
	if kind == KindRaccoon && apiPath == "/chat/completions" {
		apiPath = "/api/web/llm/v2/chat/completions"
	}
	// WorkBuddy 的聊天端点挂在 /v2 下，且 baseUrl 不应带 /v1
	if kind == KindWorkBuddy {
		if apiPath == "/chat/completions" {
			apiPath = "/v2/chat/completions"
		}
		baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	}
	// DeepSeek 网页版没有 OpenAI 的 /chat/completions：它只有一个受 PoW 保护的
	// completion 端点（SSE 体还是 patch 语义），所以路径这里直接钉死，不走 apiPath。
	if kind == KindDeepSeekW {
		apiPath = deepseekweb.PathCompletion
		baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	}
	// Qoder 的请求体不是 OpenAI 形态：要重排成它的信封（business/parameters/model_config
	// 那几段）、再套一层自定义 base64（查询参数 Encode=1），所以先在这里做完。
	// 后面的 ChatHeaders 签名覆盖的正是这条线上串，两处必须用同一份。
	var qoderWire, qoderModelKey, qoderModelSource string
	if kind == KindQoder {
		qoderModelKey = strings.TrimSpace(headerOf(extra, "X-Model-Key"))
		if qoderModelKey == "" {
			// 网关已经把真实模型名写进 body 的 model 字段，缺头时就拿它当模型 key。
			qoderModelKey = modelOf(body)
		}
		qoderModelSource = strings.TrimSpace(headerOf(extra, "X-Model-Source"))
		wire, werr := qoder.BuildChatBody(body, qoderModelKey, qoderModelSource)
		if werr != nil {
			return nil, werr
		}
		qoderWire = wire
		body = []byte(wire)
	}
	var url string
	if kind == KindTrae {
		// 这里把「客户端发来的标准 OpenAI 请求体」翻译成上游要的形状；
		// model 已经由网关改写进 body，所以不用额外传。
		opts := trae.Options{Function: headerOf(extra, "X-Allin-Function")}
		if v := headerOf(extra, "X-Allin-Context-Window"); v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				opts.PromptMaxTokens = n
			}
		}
		converted, err := trae.TransformRequestWith(body, "", opts)
		if err != nil {
			return nil, fmt.Errorf("Trae 请求体转换失败: %w", err)
		}
		body = converted
		// 走 BuildChatURL 而不是直接拼，好兼容 baseUrl 误填了 /v1 或完整端点的情况。
		url = trae.BuildChatURL(baseURL)
	} else if kind == KindQoder {
		// Qoder 的聊天端点路径与查询参数都是固定的（FetchKeys/AgentId/Encode），
		// 不接受 baseUrl 拼接，所以直接钉死整条 URL。
		url = qoder.ChatURL
	} else {
		url = strings.TrimRight(baseURL, "/") + apiPath
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	switch kind {
	case KindAnthropic:
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case KindCline:
		ca := &cline.Account{Access: key, Kind: "oauth", Name: acct.Name, UID: acct.UID, Refresh: acct.Refresh, Expires: acct.Expires}
		for k, v := range cline.Headers(ca) {
			req.Header.Set(k, v)
		}
	case KindMiniMax:
		ca := &minimax.Account{Access: key, Name: acct.Name, UserID: acct.UID, SessionID: acct.ID}
		for k, v := range minimax.Headers(ca) {
			req.Header.Set(k, v)
		}
	case KindRaccoon:
		ca := &raccoon.Account{ID: acct.ID, Name: acct.Name, UID: acct.UID,
			Access: key, Token: key, Refresh: acct.Refresh, Expires: acct.Expires}
		for k, v := range raccoon.Headers(ca) {
			req.Header.Set(k, v)
		}
	case KindLoomy:
		ca := &loomy.Account{ID: acct.ID, Name: acct.Name, Token: key, ExpiresAt: acct.Expires}
		for k, v := range loomy.Headers(ca) {
			req.Header.Set(k, v)
		}
	case KindCodex:
		ca := &codex.Account{Access: key, Refresh: acct.Refresh, AccountID: acct.UID}
		for k, v := range codex.Headers(ca) {
			req.Header.Set(k, v)
		}
	case KindCopilot:
		ca := &copilot.Account{Access: key, Refresh: acct.Refresh, Expires: acct.Expires, Name: acct.Name}
		for k, v := range copilot.ChatHeaders(ca, body) {
			req.Header.Set(k, v)
		}
	case KindTrae:
		// Trae 用 Cloud-IDE-JWT + 一整套 Windows 客户端伪装头
		id := &trae.Identity{
			DeviceID: acct.DeviceID, MachineID: acct.MachineID,
			PublicKey: acct.PublicKey, PrivateKey: acct.PrivateKey,
		}
		headers := trae.Headers(&trae.Account{
			UID: acct.UID, Token: key,
			DeviceID: acct.DeviceID, MachineID: acct.MachineID,
		})
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		_ = id
		req.Header.Del("Content-Type")
		req.Header.Set("Content-Type", "application/json")
	case KindDeepSeekW:
		// 网页版靠四个 x-client-* 指纹头 + origin/referer 认「自家前端」，
		// 缺任何一个都会被上游当成第三方调用拒掉。
		for k, v := range deepseekweb.Headers(&deepseekweb.Account{Token: key}) {
			req.Header.Set(k, v)
		}
	case KindQoder:
		// Qoder 的头是 COSY 签名信封：payload + MD5 摘要，摘要里带着编码后的请求体，
		// 所以必须把同一份 qoderWire 交给它，换成原始 body 会签名对不上。
		// ChatHeaders 内部已经删掉 Cosy-ClientIp / Cosy-MachineToken / Cosy-MachineType
		// （那三个要桌面端 runtime-info 的真实设备身份，拿 machineId 顶替会被上游 500）。
		ca := &qoder.Account{
			ID: acct.ID, Name: acct.Name, Email: acct.Email,
			UID: acct.UID, Token: key, MachineID: acct.MachineID,
		}
		headers, herr := qoder.ChatHeaders(ca, qoderWire, qoderModelKey, qoderModelSource)
		if herr != nil {
			return nil, herr
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	case KindCatPaw:
		// 妙手的头是「Cookie: X-Passport-Token=…」+ m-appkey / x-agent-version 那几个客户端标识。
		ca := &catpaw.Account{ID: acct.ID, Name: acct.Name, UID: acct.UID, Token: key}
		for k, v := range catpaw.Headers(ca) {
			req.Header.Set(k, v)
		}
	case KindWenxin:
		// 文心的登录态就是整条浏览器 Cookie；Headers 里 Cookie / Origin / Referer 一起给。
		for k, v := range wenxin.Headers(wenxin.FromCookie(key)) {
			req.Header.Set(k, v)
		}
	case KindAntigravity:
		// Code Assist 只认 Bearer + User-Agent（antigravity/hub/<ver> linux/x64）。
		ga := &antigravity.Account{
			ID: acct.ID, Name: acct.Name, Email: acct.Email,
			Access: key, Token: key, Refresh: acct.Refresh, Expires: acct.Expires,
			Project: acct.Domain,
		}
		for k, v := range antigravity.Headers(ga) {
			req.Header.Set(k, v)
		}
	case KindWorkBuddy:
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-CodeBuddy-Request", "1")
		req.Header.Set("Accept-Language", "zh-CN")
		host := "copilot.tencent.com"
		if acct != nil && acct.Domain != "" {
			host = acct.Domain
		}
		req.Header.Set("X-Domain", host)
		if acct != nil && acct.UID != "" {
			req.Header.Set("X-User-Id", acct.UID)
		}
		req.Header.Set("User-Agent", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0")
	default: // OpenAI 兼容
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("User-Agent", "allin-go/1.0")
	}

	for k, v := range extra {
		// X-Allin-* 是本项目内部使用的指令头，不能发给上游
		if strings.HasPrefix(strings.ToLower(k), "x-allin-") {
			continue
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

// DefaultBaseURL 给没填 baseUrl 的供应商兜底。
func DefaultBaseURL(kind string) string {
	kind = NormalizeKind(kind)
	switch kind {
	case KindAnthropic:
		return "https://api.anthropic.com/v1"
	case KindCatPaw:
		return catpaw.Origin
	case KindWenxin:
		return wenxin.Origin
	case KindAntigravity:
		return antigravity.ReqBase
	case KindWorkBuddy:
		return "https://copilot.tencent.com/v1"
	case KindTrae:
		return "https://trae-api-cn.mchost.guru"
	case KindCopilot:
		return "https://api.githubcopilot.com"
	case KindCodex:
		return "https://chatgpt.com/backend-api/codex"
	case KindRaccoon:
		return "https://xiaohuanxiong.com"
	case KindLoomy:
		return "https://loomyad.xunfei.cn"
	case KindQoder:
		return "https://gateway.qoder.com.cn"
	case KindCline:
		return "https://api.cline.bot/api/v1"
	case KindMiniMax:
		return "https://agent.minimax.cn/mavis/api/v1/llm/v1"
	case KindDeepSeekW:
		return deepseekweb.Origin
	default:
		return ""
	}
}

// headerOf 在供应商的固定头（config.Provider.Headers）里按名字取值，大小写不敏感。
//
// 之所以走这里取：X-Allin-* 是本项目的内部指令头，不会真的发给上游，
// 它们只用来给 BuildRequest 传「这个供应商的特殊参数」。
func headerOf(extra map[string]string, name string) string {
	for k, v := range extra {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// modelOf 取请求体里的 model 字段（网关已经把真实模型名写进去了）。
//
// Qoder 用它当兜底的模型 key：供应商没配 X-Model-Key 头时，模型名就是 key。
func modelOf(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.Model)
}
