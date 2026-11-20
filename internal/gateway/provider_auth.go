package gateway

// 第二批供应商登录入口（8 家），统一成「两段式」：
//
//	POST /api/auth/<vendor>/start         → {ok, flowId, url?, userCode?, verificationUri?, intervalMs?, deadline?, needsCode?, needsCookie?, hint?}
//	GET  /api/auth/<vendor>/poll?flowId=  → {ok, status: pending|done|failed|need_code|need_cookie, account?, error?, hint?}
//
// vendor 取值：antigravity catpaw cline codex minimax qoder raccoon wenxin
//
// 三家已有实现（workbuddy / trae / github-copilot）走的是各自的精确路径，
// ServeMux 会优先匹配它们，本文件只兜住 /api/auth/ 下的其余子路径。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"allin/internal/config"
	"allin/internal/provider"
	"allin/internal/provider/antigravity"
	"allin/internal/provider/catpaw"
	"allin/internal/provider/cline"
	"allin/internal/provider/codex"
	"allin/internal/provider/minimax"
	"allin/internal/provider/qoder"
	"allin/internal/provider/raccoon"
	"allin/internal/provider/wenxin"
)

// raccoonCallbackPort 是小浣熊回环回调的**首选**端口。
//
// 不是硬性要求：redirect 是随授权请求一起传上去的，服务端不校验端口，
// 所以占不到时换一个随机端口完全没问题。
// 优先用固定值是为了面板能提前把完整回调链接展示出来；
// 若该端口被官方客户端等程序占用，会自动退到随机端口。
const raccoonCallbackPort = 18765

// startCallbackWaiter 先试首选端口，占不到就换系统随机分配。
//
// 为什么要退化：固定端口容易被别的程序（尤其小浣熊官方客户端）占用，
// 占不到时旧代码会提示「端口没占住」并要求用户手动贴回调链接，
// 体验很差。随机端口能直接绕过这个冲突。
func startCallbackWaiter(preferred int, path string) (*codeWaiter, error) {
	if preferred > 0 {
		if cw, err := startCodeWaiter(preferred, path); err == nil {
			return cw, nil
		}
	}
	// 让内核分配空闲端口
	cw, err := startCodeWaiter(0, path)
	if err != nil {
		return nil, err
	}
	return cw, nil
}

// cbPortOf 取回调监听实际使用的端口。
func cbPortOf(cw *codeWaiter) int {
	if cw == nil || cw.ln == nil {
		return 0
	}
	if addr, ok := cw.ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// authVendor 描述一个 vendor 在配置里对应的供应商身份。
type authVendor struct {
	// ProviderID 是写进 config.providers[].id 的供应商 id（不一定等于 vendor 名）。
	ProviderID string
	// Name 是新建供应商时的显示名。
	Name string
	// Kind 是协议族（provider.KindXxx），缺了它聊天请求会走错分派。
	Kind string
	// BaseURL 留空则用 provider.DefaultBaseURL(Kind) 兜底。
	BaseURL string
	// Models 只是「新建供应商时」的兜底模型清单，已有供应商不会被覆盖。
	Models []string
}

// authVendors 是 vendor 名 → 供应商身份的对照表。
//
// 注意 minimax / qoder / wenxin 三家的 provider id 与 vendor 名不同
// （minimax-code / qoder_cn / wenxin_web），它们是包里定死的值，不能随手改。
var authVendors = map[string]authVendor{
	"antigravity": {
		ProviderID: antigravity.ProviderID,
		Name:       "Google Antigravity（登录态）",
		Kind:       provider.KindAntigravity,
		Models:     []string{"gemini-3-flash", "gemini-pro-agent", "claude-sonnet-4-6"},
	},
	"catpaw": {
		ProviderID: catpaw.ProviderID,
		Name:       "美团妙手 CatPaw（登录态）",
		Kind:       provider.KindCatPaw,
		Models:     catpawModelIDs(),
	},
	"cline": {
		ProviderID: cline.ProviderID,
		Name:       "Cline",
		Kind:       provider.KindCline,
		Models:     []string{"anthropic/claude-sonnet-4.5"},
	},
	"codex": {
		ProviderID: codex.ProviderID,
		Name:       "OpenAI Codex（登录态）",
		Kind:       provider.KindCodex,
		Models:     codex.DefaultModels,
	},
	"minimax": {
		ProviderID: minimax.ProviderID,
		Name:       "MiniMax Code（登录态）",
		Kind:       provider.KindMiniMax,
	},
	"qoder": {
		ProviderID: qoder.ProviderID,
		Name:       "Qoder 国内站（登录态）",
		Kind:       provider.KindQoder,
	},
	"raccoon": {
		ProviderID: raccoon.ProviderID,
		Name:       "商汤小浣熊（登录态）",
		Kind:       provider.KindRaccoon,
	},
	"wenxin": {
		ProviderID: wenxin.ProviderID,
		Name:       "百度文心一言网页版（登录态）",
		Kind:       provider.KindWenxin,
		Models:     append([]string(nil), wenxin.Models...),
	},
}

// catpawModelIDs 把包里的模型表转成 id 列表（妙手的 id 与展示名不同）。
func catpawModelIDs() []string {
	out := make([]string, 0, len(catpaw.Models))
	for _, m := range catpaw.Models {
		out = append(out, m.ID)
	}
	return out
}

// authVendorNames 给报错信息用。
func authVendorNames() []string {
	return []string{"antigravity", "catpaw", "cline", "codex", "minimax", "qoder", "raccoon", "wenxin"}
}

// authFlow 是一条进行中的登录流。
//
// 各家中间状态类型都不一样，这里用「每个 vendor 一个字段」的写法存着，
// poll 时按 flow.vendor 分派 —— 比塞 any 少一次类型断言，也更好读。
type authFlow struct {
	vendor   string
	provider string
	created  time.Time
	deadline time.Time
	interval time.Duration
	hint     string

	// replaceID 是「重新登录」时指定要覆盖的旧账号 ID。
	// 登录拿到的新账号 ID 可能和旧的拼法不同（比如按邮箱/UID 重新生成），
	// 只靠 ID/UID 匹配可能覆盖不到，导致账号池里留下一个死号。
	replaceID string

	ag *antigravity.Flow
	cp *catpaw.Flow
	cl *cline.DeviceFlow
	cx *codex.AuthFlow
	mm *minimax.DeviceFlow
	qd *qoder.Flow
	wx *wenxin.Flow
	rc *raccoonState
	// cb 是本地回环监听（Google / OpenAI / 小浣熊的回调都打到这里）。
	ca *codeWaiter
}

// raccoonState 是小浣熊授权时要回传比对的两项。
type raccoonState struct {
	state    string
	redirect string
}

var (
	authFlowsMu sync.Mutex
	authFlows   = map[string]*authFlow{}
)

// ---------- 本地回环回调监听 ----------

// codeWaiter 在 127.0.0.1 上等一次 OAuth 回调，把 code / state 记下来。
//
// 为什么要自己起监听：手机上的网关和浏览器是同一台机器，
// 授权页跳到 localhost 时正好能打到我们；做不到时面板还有「粘贴回调链接」兜底。
type codeWaiter struct {
	mu    sync.Mutex
	code  string
	state string
	err   error
	done  bool
	ln    net.Listener
	once  sync.Once
}

// startCodeWaiter 在指定端口上挂一个只认 path 的一次性回调接收器。
func startCodeWaiter(port int, path string) (*codeWaiter, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	cw := &codeWaiter{ln: ln}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path != "" && r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if e := q.Get("error"); e != "" {
			cw.finish("", q.Get("state"), fmt.Errorf("授权页返回错误：%s", e))
			_, _ = io.WriteString(w, "<h3>授权被拒绝，可以关闭此页。</h3>")
			return
		}
		code := strings.TrimSpace(q.Get("code"))
		if code == "" {
			_, _ = io.WriteString(w, "<h3>回调里没有 code，请回到面板重新发起。</h3>")
			return
		}
		cw.finish(code, q.Get("state"), nil)
		_, _ = io.WriteString(w, "<h3>授权完成，可以关闭此页并回到面板。</h3>")
	})}
	go func() { _ = srv.Serve(ln) }()
	return cw, nil
}

// finish 记录一次结果（只认第一次），并立刻关掉监听。
func (c *codeWaiter) finish(code, state string, err error) {
	c.mu.Lock()
	if !c.done {
		c.code, c.state, c.err, c.done = code, state, err, true
	}
	c.mu.Unlock()
	c.close()
}

func (c *codeWaiter) close() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		if c.ln != nil {
			_ = c.ln.Close()
		}
	})
}

// result 取一次结果；done=false 表示还没收到回调。
func (c *codeWaiter) result() (code, state string, err error, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code, c.state, c.err, c.done
}

func (f *authFlow) closeWaiter() {
	if f != nil && f.ca != nil {
		f.ca.close()
	}
}

// ---------- 路由 ----------

// handleProviderAuth 是 /api/auth/<vendor>/<start|poll> 的统一入口。
func (s *Server) handleProviderAuth(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/auth/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok":        false,
			"error":     "路径应为 /api/auth/<vendor>/start 或 /api/auth/<vendor>/poll",
			"supported": authVendorNames(),
		})
		return
	}
	switch vendor, action := parts[0], parts[1]; action {
	case "start":
		s.handleAuthStart(w, r, vendor)
	case "poll":
		s.handleAuthPoll(w, r, vendor)
	case "token":
		s.handleAuthToken(w, r, vendor)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "不支持的动作: " + action})
	}
}

// handleAuthStart 起一条登录流。
func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request, vendor string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "只支持 POST"})
		return
	}
	if _, ok := authVendors[vendor]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "不支持的供应商: " + vendor, "supported": authVendorNames(),
		})
		return
	}
	pruneAuthFlows()
	// 同一家重新发起视为「旧的作废」：先放掉它占着的本地回调端口，
	// 否则第二次 start 会 bind: address already in use。
	dropAuthFlowsOf(vendor)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// 重新登录：请求可带 replaceAccountId，指定要覆盖的旧账号。
	var startIn struct {
		ReplaceAccountID string `json:"replaceAccountId"`
	}
	_ = decodeJSON(r, &startIn)

	flow, payload, err := startAuthFlow(ctx, vendor)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	flowID := newAuthFlowID()
	authFlowsMu.Lock()
	authFlows[flowID] = flow
	authFlowsMu.Unlock()

	flow.replaceID = strings.TrimSpace(startIn.ReplaceAccountID)
	payload["ok"] = true
	payload["flowId"] = flowID
	payload["vendor"] = vendor
	payload["provider"] = flow.provider
	payload["deadline"] = flow.deadline.Unix()
	if flow.interval > 0 {
		payload["intervalMs"] = flow.interval.Milliseconds()
	}
	// 各分支已经把自己的说明拼进 payload.hint 了，这里只在缺失时补 flow.hint，
	// 不要再合并一次（否则「端口没占住」这类提示会出现两遍）。
	if authString(payload["hint"]) == "" && flow.hint != "" {
		payload["hint"] = flow.hint
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleAuthPoll 查一次登录状态；拿到账号就写进配置。
func (s *Server) handleAuthPoll(w http.ResponseWriter, r *http.Request, vendor string) {
	q := r.URL.Query()
	flowID := strings.TrimSpace(q.Get("flowId"))
	code := strings.TrimSpace(q.Get("code"))
	cookie := strings.TrimSpace(q.Get("cookie"))
	// 兼容 POST body（与 github-copilot 那条保持一致的使用习惯）
	if flowID == "" || (code == "" && cookie == "") {
		var body struct {
			FlowID string `json:"flowId"`
			Code   string `json:"code"`
			Cookie string `json:"cookie"`
		}
		if r.Body != nil && decodeJSON(r, &body) == nil {
			if flowID == "" {
				flowID = strings.TrimSpace(body.FlowID)
			}
			if code == "" {
				code = strings.TrimSpace(body.Code)
			}
			if cookie == "" {
				cookie = strings.TrimSpace(body.Cookie)
			}
		}
	}
	if flowID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 flowId"})
		return
	}

	authFlowsMu.Lock()
	flow := authFlows[flowID]
	authFlowsMu.Unlock()
	if flow == nil || flow.vendor != vendor {
		// 用 200 + status=failed 而不是 404：面板统一按 status 处理，
		// 404 会走「网络异常，稍后重试」分支，导致对着一个已失效的流无限重试。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": "failed", "provider": authVendors[vendor].ProviderID,
			"error": "登录流不存在或已过期（服务重启后需要重新发起）",
		})
		return
	}
	if !flow.deadline.IsZero() && time.Now().After(flow.deadline.Add(30*time.Second)) {
		authFlowsMu.Lock()
		delete(authFlows, flowID)
		authFlowsMu.Unlock()
		flow.closeWaiter()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": "failed", "provider": flow.provider,
			"error": "登录流已超时，请重新发起",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	status, done, msg := pollAuthFlow(ctx, flow, code, cookie)
	if status == "done" && done != nil {
		authFlowsMu.Lock()
		delete(authFlows, flowID)
		authFlowsMu.Unlock()
		flow.closeWaiter()

		if err := s.saveAuthAccountReplacing(vendor, done.acc, done.models, flow.replaceID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "status": "failed", "provider": flow.provider,
				"error": "登录成功但保存配置失败: " + err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"status":   "done",
			"account":  done.name,
			"provider": flow.provider,
			"hint":     "已写入配置，模型名可用 " + flow.provider + "/<模型>",
		})
		return
	}

	out := map[string]any{"ok": status != "failed", "status": status, "provider": flow.provider}
	switch status {
	case "failed":
		out["error"] = msg
	case "need_code":
		out["needsCode"] = true
		out["hint"] = msg
	case "need_cookie":
		out["needsCookie"] = true
		out["hint"] = msg
	default:
		if msg != "" {
			out["hint"] = msg
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- 起流程 ----------

// startAuthFlow 按 vendor 起一条登录流，并给出面板首屏要展示的内容。
func startAuthFlow(ctx context.Context, vendor string) (*authFlow, map[string]any, error) {
	now := time.Now()
	out := map[string]any{}
	switch vendor {
	case "antigravity":
		f, err := antigravity.StartLogin()
		if err != nil {
			return nil, nil, err
		}
		af := &authFlow{
			vendor: vendor, provider: antigravity.ProviderID,
			created: now, deadline: now.Add(10 * time.Minute), ag: f,
		}
		// Google 只接受 localhost:1456/oauth-callback；能占住端口就自动收码，
		// 占不住（上一轮没退干净等）就退回「把回调链接贴回来」。
		if cw, lerr := startCodeWaiter(antigravity.CallbackPort, antigravity.CallbackPath); lerr == nil {
			af.ca = cw
		} else {
			af.hint = fmt.Sprintf("本机回调端口 %d 没占住（%v），授权后请把浏览器地址栏的完整链接贴回来",
				antigravity.CallbackPort, lerr)
		}
		out["url"] = f.AuthURL
		out["needsCode"] = true
		out["hint"] = hintOf(af.hint, "用浏览器打开 url 完成 Google 授权；本机收到回调会自动换票写入账号。")
		return af, out, nil

	case "catpaw":
		f, err := catpaw.StartLogin(ctx)
		if err != nil {
			return nil, nil, err
		}
		af := &authFlow{
			vendor: vendor, provider: catpaw.ProviderID,
			created: now, deadline: f.Deadline, cp: f, interval: 3 * time.Second,
		}
		out["url"] = f.EntryURL
		out["hint"] = "用浏览器打开 url 登录妙手（CatPaw）；服务端会轮询 sid，登录完成自动写入账号。"
		return af, out, nil

	case "cline":
		f, err := cline.StartDeviceFlow(ctx)
		if err != nil {
			return nil, nil, err
		}
		af := &authFlow{
			vendor: vendor, provider: cline.ProviderID,
			created: now, deadline: f.Deadline, cl: f, interval: f.Interval,
		}
		out["userCode"] = f.UserCode
		out["verificationUri"] = f.VerificationURI
		out["verificationUriComplete"] = f.VerificationURIComplete
		out["hint"] = "打开 verificationUri 输入 userCode，完成 Cline（WorkOS）设备码授权。"
		return af, out, nil

	case "codex":
		f, err := codex.StartAuth()
		if err != nil {
			return nil, nil, err
		}
		af := &authFlow{
			vendor: vendor, provider: codex.ProviderID,
			created: now, deadline: now.Add(10 * time.Minute), cx: f,
		}
		// Codex 的回调固定打到 localhost:1455/auth/callback（OpenAI 侧注册的就是它），
		// 端口占不住时只能靠用户粘贴链接。
		if cw, lerr := startCodeWaiter(codex.CallbackPort, "/auth/callback"); lerr == nil {
			af.ca = cw
		} else {
			af.hint = fmt.Sprintf("本机回调端口 %d 没占住（%v），授权后请把浏览器地址栏的完整链接贴回来",
				codex.CallbackPort, lerr)
		}
		out["url"] = f.AuthURL
		out["needsCode"] = true
		out["hint"] = hintOf(af.hint, "用浏览器打开 url 登录 ChatGPT；本机收到回调会自动换票写入账号。")
		return af, out, nil

	case "minimax":
		f, err := minimax.StartDeviceFlow(ctx)
		if err != nil {
			return nil, nil, err
		}
		af := &authFlow{
			vendor: vendor, provider: minimax.ProviderID,
			created: now, deadline: f.Deadline, mm: f, interval: f.Interval,
		}
		out["userCode"] = f.UserCode
		out["verificationUri"] = f.VerificationURI
		out["verificationUriComplete"] = f.VerificationURIComplete
		out["hint"] = "打开 verificationUri 输入 userCode，完成 MiniMax 设备码授权。"
		return af, out, nil

	case "qoder":
		f, err := qoder.StartDeviceFlow()
		if err != nil {
			return nil, nil, err
		}
		af := &authFlow{
			vendor: vendor, provider: qoder.ProviderID,
			created: now, deadline: f.Deadline, qd: f, interval: f.Interval,
		}
		out["url"] = f.AuthorizeURL
		out["hint"] = "打开授权页选择要授权的账号，完成 Qoder 设备码授权（无需粘贴）。"
		return af, out, nil

	case "raccoon":
		state := newAuthFlowID()

		// 先起监听拿到真实端口，再拼 redirect ——
		// 端口可能不是首选值（被占用时退到随机），必须先绑定后取值。
		cw, lerr := startCallbackWaiter(raccoonCallbackPort, raccoon.CallbackPath)
		port := raccoonCallbackPort
		if lerr == nil {
			if actual := cbPortOf(cw); actual > 0 {
				port = actual
			}
		}
		redirect := fmt.Sprintf("http://127.0.0.1:%d%s", port, raccoon.CallbackPath)

		af := &authFlow{
			vendor: vendor, provider: raccoon.ProviderID,
			created: now, deadline: now.Add(10 * time.Minute),
			rc: &raccoonState{state: state, redirect: redirect},
		}
		if lerr == nil {
			af.ca = cw
		} else {
			af.hint = fmt.Sprintf("本机回调端口没占住（%v），授权后请把浏览器地址栏的完整链接贴回来", lerr)
		}
		out["url"] = raccoon.AuthorizeURLFor(state, redirect)
		out["needsCode"] = true
		out["hint"] = hintOf(af.hint, "用浏览器打开 url 授权小浣熊；本机收到回调会自动换票写入账号。")
		return af, out, nil

	case "wenxin":
		f, err := wenxin.StartLogin()
		if err != nil {
			return nil, nil, err
		}
		af := &authFlow{
			vendor: vendor, provider: wenxin.ProviderID,
			created: now, deadline: now.Add(10 * time.Minute), wx: f,
		}
		out["url"] = f.URL
		out["needsCookie"] = true
		out["hint"] = "文心没有服务端轮询通道（包里 PollLogin 恒返回未实现）：在这个浏览器登录百度后，" +
			"把 chat.baidu.com 的整条 Cookie 贴到下面的框里提交。"
		return af, out, nil
	}
	return nil, nil, fmt.Errorf("不支持的供应商: %s", vendor)
}

// ---------- 轮询 ----------

// authDone 是「已经拿到账号」的统一结果。
type authDone struct {
	acc    *config.Account
	name   string
	models []string
}

// pollAuthFlow 查一次状态。
//
// 返回值 status 取 pending / failed / need_code / need_cookie / done；
// 非 done 时 msg 是给面板看的说明（failed 里它就是错误原因）。
func pollAuthFlow(ctx context.Context, f *authFlow, code, cookie string) (status string, done *authDone, msg string) {
	switch f.vendor {
	case "antigravity":
		af := f.ag
		got, st0, cerr, ok := takeCallback(f, code)
		if !ok {
			return "pending", nil, hintOf(f.hint, "等待 Google 授权回调…")
		}
		if cerr != nil {
			return "failed", nil, cerr.Error()
		}
		// 包自带的 SubmitCallback 同时认「完整回调链接」和「裸 code」
		c, st, err := antigravity.SubmitCallback(got)
		if err != nil {
			return "failed", nil, err.Error()
		}
		if st == "" {
			st = st0
		}
		if st != "" && st != af.State {
			return "failed", nil, "回调里的 state 与本次登录不一致，请重新发起"
		}
		acc := &antigravity.Account{}
		if err := antigravity.ExchangeCode(ctx, acc, c, af.Verifier, af.Redirect); err != nil {
			return "failed", nil, err.Error()
		}
		return "done", antigravityDone(ctx, acc), ""

	case "catpaw":
		acc, err := catpaw.PollLogin(ctx, f.cp)
		if err != nil {
			return "failed", nil, err.Error()
		}
		if acc == nil {
			return "pending", nil, "等待你在浏览器完成妙手登录…"
		}
		return "done", catpawDone(acc), ""

	case "cline":
		acc, err := cline.PollDeviceFlow(ctx, f.cl)
		switch {
		case err == nil:
			return "done", clineDone(acc), ""
		case errors.Is(err, cline.ErrPending):
			return "pending", nil, "等待你在授权页确认…"
		case errors.Is(err, cline.ErrSlowDown):
			return "pending", nil, "上游要求放慢轮询，稍后再查…"
		default:
			return "failed", nil, err.Error()
		}

	case "codex":
		c, st, err, ok := takeCallback(f, code)
		if !ok {
			return "pending", nil, hintOf(f.hint, "等待 ChatGPT 授权回调…")
		}
		if err != nil {
			return "failed", nil, err.Error()
		}
		if st != "" && st != f.cx.State {
			return "failed", nil, "回调里的 state 与本次登录不一致，请重新发起"
		}
		acc, err := codex.ExchangeCode(ctx, c, f.cx.Verifier)
		if err != nil {
			return "failed", nil, err.Error()
		}
		return "done", codexDone(acc), ""

	case "minimax":
		acc, err := minimax.PollDeviceFlow(ctx, f.mm)
		switch {
		case err == nil:
			return "done", minimaxDone(acc), ""
		case errors.Is(err, minimax.ErrPending):
			return "pending", nil, "等待你在授权页确认…"
		case errors.Is(err, minimax.ErrSlowDown):
			return "pending", nil, "上游要求放慢轮询，稍后再查…"
		default:
			return "failed", nil, err.Error()
		}

	case "qoder":
		res, err := qoder.PollDeviceFlow(ctx, f.qd)
		if err != nil {
			return "failed", nil, err.Error()
		}
		if res == nil {
			return "pending", nil, "等待你在 Qoder 授权页选择账号…"
		}
		if res.Status == "ready" && res.Account != nil {
			return "done", qoderDone(ctx, res.Account), ""
		}
		m := "等待你在 Qoder 授权页选择账号…"
		if res.Error != "" {
			m = "等待中（" + res.Error + "）"
		}
		return "pending", nil, m

	case "raccoon":
		c, st, err, ok := takeCallback(f, code)
		if !ok {
			return "pending", nil, hintOf(f.hint, "等待你在浏览器授权小浣熊…")
		}
		if err != nil {
			return "failed", nil, err.Error()
		}
		if st != "" && st != f.rc.state {
			return "failed", nil, "回调里的 state 与本次登录不一致，请重新发起"
		}
		acc, err := raccoon.ExchangeCode(ctx, c)
		if err != nil {
			return "failed", nil, err.Error()
		}
		return "done", raccoonDone(ctx, acc), ""

	case "wenxin":
		if cookie == "" {
			return "need_cookie", nil, "文心没有服务端轮询：登录百度后把 chat.baidu.com 的整条 Cookie 贴进来提交。"
		}
		acc := wenxin.FromCookie(cookie)
		if strings.TrimSpace(acc.Cookie) == "" {
			return "failed", nil, "Cookie 为空"
		}
		return "done", wenxinDone(acc), ""
	}
	return "failed", nil, "不支持的供应商: " + f.vendor
}

// takeCallback 把「本地回环收到的」和「用户粘贴的」统一成一份回调结果。
//
// pasted 非空时以粘贴内容为准（用户手动兜底优先，避免本地监听没收到时卡死）；
// 否则读本地监听；两者都没有时 done=false，调用方按 pending 处理。
func takeCallback(f *authFlow, pasted string) (code, state string, err error, done bool) {
	if s := strings.TrimSpace(pasted); s != "" {
		c, st, perr := splitCallbackCode(s)
		return c, st, perr, true
	}
	if f == nil || f.ca == nil {
		return "", "", nil, false
	}
	return f.ca.result()
}

// splitCallbackCode 把「完整回调链接」或「裸 code / code=xxx&state=yyy」统一成 code + state。
func splitCallbackCode(input string) (code, state string, err error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", "", errors.New("回调链接/授权码为空")
	}
	if strings.HasPrefix(strings.ToLower(s), "http") {
		// 有的浏览器把 query 放在 # 之后，两种都认
		if i := strings.Index(s, "#"); i >= 0 && strings.Contains(s[i:], "code=") {
			s = s[:i] + "?" + s[i+1:]
		}
		u, perr := url.Parse(s)
		if perr != nil {
			return "", "", fmt.Errorf("回调链接解析失败: %w", perr)
		}
		q := u.Query()
		if e := q.Get("error"); e != "" {
			return "", "", fmt.Errorf("授权页返回错误：%s", e)
		}
		c := q.Get("code")
		if c == "" {
			return "", "", errors.New("回调链接里没有 code")
		}
		return c, q.Get("state"), nil
	}
	if strings.Contains(s, "=") {
		if v, perr := url.ParseQuery(strings.TrimPrefix(s, "?")); perr == nil {
			if c := v.Get("code"); c != "" {
				return c, v.Get("state"), nil
			}
		}
	}
	return s, "", nil
}

// ---------- 账号字段映射 ----------

// antigravityDone 把 Google 账号映射成配置账号。
//
// 关键映射（与 provider.BuildRequest / gateway.runAntigravity 对齐）：
// access→Access、refresh→Refresh、project→Domain（聊天时当 Cloud 项目 id 用）。
func antigravityDone(ctx context.Context, a *antigravity.Account) *authDone {
	enabled := true
	name := authFirst(a.Email, a.Name, "antigravity")
	acc := &config.Account{
		ID:      "antigravity-" + name,
		Name:    name,
		Type:    "google",
		Enabled: &enabled,
		Access:  a.Access,
		Refresh: a.Refresh,
		UID:     authFirst(a.Email, a.Name),
		Email:   a.Email,
		Domain:  a.Project,
		Expires: a.Expires,
	}
	label := name
	if a.Project != "" {
		label = name + " / 项目 " + a.Project
	}
	if a.Plan != "" {
		label += " / " + a.Plan
	}
	var models []string
	// 项目 id 拿到时才值得拉一次目录（失败就忽略，用兜底清单）
	if a.Project != "" {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if list, err := antigravity.FetchModels(dctx, a, a.Access, a.Project); err == nil {
			for _, m := range list {
				if strings.TrimSpace(m.ID) != "" {
					models = append(models, m.ID)
				}
			}
		}
	}
	return &authDone{acc: acc, name: label, models: models}
}

// catpawDone 妙手：token 就是 Cookie 里的 X-Passport-Token。
func catpawDone(a *catpaw.Account) *authDone {
	enabled := true
	name := authFirst(a.Name, a.UID, "妙手账号")
	acc := &config.Account{
		ID:      "catpaw-" + authFirst(a.UID, name),
		Name:    name,
		Type:    "token",
		Enabled: &enabled,
		Access:  a.Token,
		UID:     a.UID,
	}
	return &authDone{acc: acc, name: name}
}

// clineDone Cline（WorkOS 设备流）：access 必须带 workos: 前缀，交给包里处理。
func clineDone(a *cline.Account) *authDone {
	enabled := true
	name := authFirst(a.Name, a.Email, a.UID, "Cline 账号")
	acc := &config.Account{
		ID:      "cline-" + authFirst(a.UID, a.Email, name),
		Name:    name,
		Type:    authFirst(a.Kind, "oauth"),
		Enabled: &enabled,
		Access:  a.Access,
		Refresh: a.Refresh,
		UID:     a.UID,
		Email:   a.Email,
		Expires: a.Expires,
	}
	return &authDone{acc: acc, name: name}
}

// codexDone Codex：config.UID 存 chatgpt_account_id（聊天时进 chatgpt-account-id 头）。
func codexDone(a *codex.Account) *authDone {
	enabled := true
	name := authFirst(a.Email, a.Name, a.AccountID, "Codex 账号")
	acc := &config.Account{
		ID:      "codex-" + name,
		Name:    name,
		Type:    authFirst(a.Type, "oauth"),
		Enabled: &enabled,
		Access:  a.AccessToken(),
		Refresh: a.Refresh,
		UID:     a.AccountID,
		Email:   a.Email,
		Expires: a.Expires,
	}
	return &authDone{acc: acc, name: name}
}

// minimaxDone MiniMax Code：UserID 是签名接口要的 realUserID。
func minimaxDone(a *minimax.Account) *authDone {
	enabled := true
	name := authFirst(a.Name, a.Email, a.UserID, "MiniMax 账号")
	acc := &config.Account{
		ID:      "minimax-" + authFirst(a.UserID, name),
		Name:    name,
		Type:    "oauth",
		Enabled: &enabled,
		Access:  a.Access,
		Refresh: a.Refresh,
		UID:     a.UserID,
		Email:   a.Email,
		Expires: a.Expires,
	}
	return &authDone{acc: acc, name: name}
}

// qoderDone Qoder：machineId 必须落盘（签 COSY 要用），expiresAt 是毫秒。
func qoderDone(ctx context.Context, a *qoder.Account) *authDone {
	enabled := true
	name := authFirst(a.Name, a.Email, a.UID, "Qoder 账号")
	acc := &config.Account{
		ID:        "qoder-" + authFirst(a.Email, a.UID, name),
		Name:      name,
		Type:      "oauth",
		Enabled:   &enabled,
		Access:    a.AccessToken(),
		Refresh:   a.RefreshTokenValue(),
		UID:       a.UID,
		Email:     a.Email,
		Expires:   a.ExpiresAt,
		MachineID: a.MachineID,
	}
	var models []string
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if list, err := qoder.Models(dctx, a); err == nil {
		models = list
	}
	return &authDone{acc: acc, name: name, models: models}
}

// raccoonDone 小浣熊：access 是 JWT，refresh 是一次性的。
func raccoonDone(ctx context.Context, a *raccoon.Account) *authDone {
	enabled := true
	name := authFirst(a.Name, a.UID, "小浣熊账号")
	acc := &config.Account{
		ID:      "raccoon-" + authFirst(a.UID, name),
		Name:    name,
		Type:    authFirst(a.Type, "oauth"),
		Enabled: &enabled,
		Access:  a.Access,
		Refresh: a.Refresh,
		UID:     a.UID,
		Expires: a.Expires,
	}
	var models []string
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if list, err := raccoon.Models(dctx, a); err == nil {
		models = list
	}
	return &authDone{acc: acc, name: name, models: models}
}

// wenxinDone 文心：登录态就是整条浏览器 Cookie（access 存 Cookie 串）。
func wenxinDone(a *wenxin.Account) *authDone {
	enabled := true
	name := authFirst(a.Name, a.UID, "文心网页版")
	// Cookie 串里没有可读身份，用它的短哈希当账号 id：同一条 Cookie 重复登录会互相覆盖，
	// 换号登录则是新账号。
	sum := sha256.Sum256([]byte(strings.TrimSpace(a.Cookie)))
	fingerprint := hex.EncodeToString(sum[:4])
	acc := &config.Account{
		ID:      "wenxin-" + fingerprint,
		Name:    name,
		Type:    "cookie",
		Enabled: &enabled,
		Access:  a.Cookie,
		UID:     a.UID,
	}
	return &authDone{acc: acc, name: name}
}

// ---------- 写配置 ----------

// saveAuthAccount 把登录得到的账号写进配置（不指定替换目标）。
func (s *Server) saveAuthAccount(vendor string, a *config.Account, models []string) error {
	return s.saveAuthAccountReplacing(vendor, a, models, "")
}

// saveAuthAccountReplacing 是带「指定替换旧账号」的版本。
//
// replaceID 非空时，先把该 ID 的旧账号从池里摘掉再写入新账号，
// 这样即便新旧账号 ID 拼法不同，也不会留下一个永远 401 的死号。
func (s *Server) saveAuthAccountReplacing(vendor string, a *config.Account, models []string, replaceID string) error {
	v, ok := authVendors[vendor]
	if !ok {
		return fmt.Errorf("未知供应商: %s", vendor)
	}
	if a == nil {
		return errors.New("账号为空")
	}
	cfg := s.store.Snapshot()
	p := cfg.FindProvider(v.ProviderID)
	if p == nil {
		base := v.BaseURL
		if base == "" {
			base = provider.DefaultBaseURL(v.Kind)
		}
		p = &config.Provider{
			ID:      v.ProviderID,
			Name:    v.Name,
			Kind:    v.Kind,
			BaseURL: base,
			// 复制一份，别把上表里的切片挂进配置
			Models: append([]string(nil), v.Models...),
		}
		cfg.Providers = append(cfg.Providers, p)
	}
	// 指定了替换目标：先按 ID 摘掉旧账号（新账号稍后写入）
	if replaceID != "" {
		kept := make([]*config.Account, 0, len(p.Accounts))
		for _, old := range p.Accounts {
			if old != nil && old.ID == replaceID {
				continue
			}
			kept = append(kept, old)
		}
		p.Accounts = kept
	}
	// 同一账号（ID 相同或 UID 相同）重复登录就覆盖，避免账号池里堆重复项
	replaced := false
	for i, old := range p.Accounts {
		if old == nil {
			continue
		}
		if old.ID == a.ID || (a.UID != "" && old.UID == a.UID) {
			p.Accounts[i] = a
			replaced = true
			break
		}
	}
	if !replaced {
		p.Accounts = append(p.Accounts, a)
	}
	// 模型清单：只在原来为空时补，绝不覆盖用户手动改过的列表
	if len(p.Models) == 0 && len(models) > 0 {
		p.Models = append([]string(nil), models...)
	}
	if err := s.store.Save(cfg); err != nil {
		return err
	}
	// 重新登录成功后，把账号池里的「需重新登录」标记和冷却一并清掉，
	// 否则新 token 已经能用，选号逻辑却还把它当死号排除。
	s.pool.MarkNeedRelogin(p.ID, a.ID, false)
	s.pool.Reset(p.ID, a.ID)
	return nil
}

// ---------- 杂项 ----------

// pruneAuthFlows 清掉过期登录流（顺手关掉它们的本地监听）。
func pruneAuthFlows() {
	now := time.Now()
	authFlowsMu.Lock()
	defer authFlowsMu.Unlock()
	for id, f := range authFlows {
		if !f.deadline.IsZero() && now.After(f.deadline.Add(time.Minute)) {
			f.closeWaiter()
			delete(authFlows, id)
		}
	}
}

// dropAuthFlowsOf 作废某一家所有进行中的登录流（重新发起时用）。
func dropAuthFlowsOf(vendor string) {
	authFlowsMu.Lock()
	defer authFlowsMu.Unlock()
	for id, f := range authFlows {
		if f.vendor == vendor {
			f.closeWaiter()
			delete(authFlows, id)
		}
	}
}

// newAuthFlowID 生成 8 字节随机 hex，作为 flowId / state。
func newAuthFlowID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// hintOf 把多段说明拼成一句话（丢掉空段）。
func hintOf(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "；")
}

// authFirst 取第一个非空字符串。
func authFirst(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// authString 把 payload 里的任意值当字符串取（拼 hint 用）。
func authString(v any) string {
	s, _ := v.(string)
	return s
}

// handleAuthToken 用现成 token 直接建账号（目前只支持小浣熊）。
//
//	POST /api/auth/raccoon/token  {"token":"eyJ..."}
//
// 用途：用户已在网页版登录时，浏览器里就有可用的 access token，
// 贴进来即可，不必再走一遍授权码流程。
// 建好后会立刻发一次真实请求验证可用性，避免存进一个死 token。
func (s *Server) handleAuthToken(w http.ResponseWriter, r *http.Request, vendor string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	if vendor != "raccoon" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "该供应商不支持直接粘贴 token，请走 " + vendor + "/start",
		})
		return
	}

	var in struct {
		Token  string `json:"token"`
		Name   string `json:"name"`
		Verify *bool  `json:"verify"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
		return
	}

	acc, err := raccoon.AccountFromToken(in.Token)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if strings.TrimSpace(in.Name) != "" {
		acc.Name = strings.TrimSpace(in.Name)
		acc.ID = acc.Name
	}

	// 默认验证一次，确保这个 token 真能用
	doVerify := in.Verify == nil || *in.Verify
	verified := false
	verifyNote := ""
	if doVerify {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := raccoon.Ping(ctx, acc); err != nil {
			verifyNote = err.Error()
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": "token 验证失败：" + err.Error(),
				"hint": "确认是 access token 而不是 refresh token；也确认没复制漏字符。",
			})
			return
		}
		verified = true
	}

	if err := s.saveRaccoonAccount(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"account":  acc.ID,
		"name":     acc.Name,
		"uid":      acc.UID,
		"expires":  acc.Expires,
		"verified": verified,
		"note":     verifyNote,
	})
}

// saveRaccoonAccount 把小浣熊账号写进配置（同 ID 覆盖）。
func (s *Server) saveRaccoonAccount(acc *raccoon.Account) error {
	cfg := s.store.Snapshot()
	p := cfg.FindProvider(raccoon.ProviderID)
	if p == nil {
		p = &config.Provider{ID: raccoon.ProviderID, Name: "商汤小浣熊", Kind: raccoon.ProviderID}
		cfg.Providers = append(cfg.Providers, p)
	}
	// 同 ID 或同 UID 视为同一个账号，覆盖
	var target *config.Account
	for _, a := range p.Accounts {
		if a == nil {
			continue
		}
		if a.ID == acc.ID || (acc.UID != "" && a.UID == acc.UID) {
			target = a
			break
		}
	}
	if target == nil {
		target = &config.Account{ID: acc.ID, AddedAt: time.Now().UnixMilli()}
		p.Accounts = append(p.Accounts, target)
	}
	target.Name = acc.Name
	target.Access = acc.Access
	target.Refresh = acc.Refresh
	target.UID = acc.UID
	target.Type = acc.Type
	target.Expires = acc.Expires
	target.AddedAt = acc.AddedAt
	return s.store.Save(cfg)
}
