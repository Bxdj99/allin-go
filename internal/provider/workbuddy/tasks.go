// tasks.go WorkBuddy「任务中心自动化」的全部非签到任务。
//
// 逻辑对照 wb2api（internal/upstream/*.go + internal/scheduler/*.go）逐条抄过来，
// 只依赖标准库。这里把上游散在几个文件里的端点集中成一张表，好处是
// 「哪个任务走哪个域」一眼能看见——这是最容易搞错的地方：
//
//	billing 域（CN 走 https://www.codebuddy.cn，global 走 https://www.workbuddy.ai）
//	    /billing/meter/claim-gift
//	    /billing/meter/claim-compensation
//	    /billing/meter/compensation-status   (POST)
//	    /v2/report                           ← 活跃上报（注意带 /v2）
//	    /v2/billing/meter/daily-checkin      ← 签到，已有实现，本文件不重写
//
//	growth 域（CN 走 https://copilot.tencent.com，global 走 https://www.workbuddy.ai）
//	    /activity/growth/streak
//	    /activity/growth/redeem
//	    /activity/growth/heatmap
//	    /activity/growth/makeup-cards/use
//	    /activity/growth/buddy/info
//	    /activity/growth/buddy/agreement
//	    /activity/growth/buddy/first
//	    /activity/growth/buddy/travel/status
//	    /activity/growth/buddy/travel/depart
//	    /activity/growth/buddy/travel/claim
//	    /activity/growth/lottery/summary
//	    /activity/growth/lottery/draw
//
// 请求头统一走 billing 那套（Authorization / X-CodeBuddy-Request / X-User-Id /
// X-Domain / X-Machine-ID / X-Session-ID …）：实测 growth 域与 billing 域吃同一套头，
// wb2api 的 growthJSON 也是复用 BillingHeaders。
package workbuddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------- 常量：域与路径 ----------

const (
	// 出站客户端版本段。对齐 wb2api 的 defaultClientVersion / defaultCliVersion，
	// 上游对这些头不严格校验，但缺了会显得不像官方客户端。
	// 客户端版本号要和官方当前版本一致。
	// 报旧版本（之前是 5.5.4）会被上游判为可疑客户端，
	// 与缺失指纹头叠加起来就更容易触发渠道校验。
	wbClientVersion = "5.9.3"
	wbCliVersion    = "2.165.0"

	// CN 三个 base。注意 billing 与 growth 不是同一个域：
	// billing 在 codebuddy.cn，growth 在 copilot.tencent.com。
	baseBillingCN = "https://www.codebuddy.cn"
	baseGrowthCN  = "https://copilot.tencent.com"
	baseRefererCN = "https://www.codebuddy.cn"
	// global 所有域合成一个。
	baseGlobal = "https://www.workbuddy.ai"

	pathClaimGift          = "/billing/meter/claim-gift"
	pathClaimCompensation  = "/billing/meter/claim-compensation"
	pathCompensationStatus = "/billing/meter/compensation-status"
	pathReport             = "/v2/report"

	pathStreak      = "/activity/growth/streak"
	pathRedeem      = "/activity/growth/redeem"
	pathHeatmap     = "/activity/growth/heatmap"
	pathMakeupUse   = "/activity/growth/makeup-cards/use"
	pathBuddyInfo   = "/activity/growth/buddy/info"
	pathBuddyAgree  = "/activity/growth/buddy/agreement"
	pathBuddyFirst  = "/activity/growth/buddy/first"
	pathTravelStat  = "/activity/growth/buddy/travel/status"
	pathTravelGo    = "/activity/growth/buddy/travel/depart"
	pathTravelClaim = "/activity/growth/buddy/travel/claim"
	pathLotterySum  = "/activity/growth/lottery/summary"
	pathLotteryDraw = "/activity/growth/lottery/draw"

	// travelLocationID 派出地点。4 个地点收益/时长区间完全相同，取 4 只是照抄
	// wb2api 的既有值，没有最优解。
	travelLocationID = 4

	// buddyTaskIncompleteMarker 领养门槛未达的业务错误关键词（HTTP 400 时出现）。
	buddyTaskIncompleteMarker = "first_buddy task not completed yet"
)

// cstZone 上游自然日按 00:00 CST 重置。中国无夏令时，固定 +8 即可，不依赖 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// ---------- 结果语义 ----------

// Outcome 是单个任务的一次执行结论。
//
// 之所以不只用 error 表达，是因为「已领过 / 活动没开」在上游都是 code != 0
// 的业务错误，和「真的失败」长得一模一样。wb2api 的 compensation.go 专门为此
// 拆出了状态查询（issue #189），这里保留同样的区分度。
type Outcome struct {
	// Status 见下方 Outcome* 常量。
	Status string `json:"status"`
	// Message 人类可读说明（上游 msg 或本地判定原因）。
	Message string `json:"message,omitempty"`
	// Credit 本次实际到账积分（部分任务有）。
	Credit int64 `json:"credit,omitempty"`
	// Data 上游 data 原文（查询/抽奖类任务用它承载细节）。
	Data map[string]any `json:"data,omitempty"`
}

// Status 取值。前端与脚本按这些字面量分支，别随手改。
const (
	// OutcomeOK 真的干成了（签到到账、领到补偿、派出成功…）。
	OutcomeOK = "ok"
	// OutcomeAlready 今天已经做过了（签到重复、礼包已领、旅行已达当日上限）。
	OutcomeAlready = "already"
	// OutcomeSkipped 前置条件不满足，主动跳过（活动未开、额度 0、无猫、
	// 在途不能领、没抽奖次数…）。
	OutcomeSkipped = "skipped"
	// OutcomeLocked 被上游明确拒绝但属预期（连登档位未解锁 403）。
	OutcomeLocked = "locked"
	// OutcomeFailed 真失败（网络、鉴权、5xx、解析不了）。
	OutcomeFailed = "failed"
)

func done(status, msg string) Outcome { return Outcome{Status: status, Message: msg} }

func failed(err error) Outcome {
	if err == nil {
		return Outcome{Status: OutcomeFailed}
	}
	return Outcome{Status: OutcomeFailed, Message: err.Error()}
}

// ---------- 上游错误 ----------

// apiError 是上游返回的错误：HTTP 非 2xx，或 HTTP 200 但业务 code != 0。
type apiError struct {
	Status int
	Code   int
	Msg    string
}

func (e *apiError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
	}
	return fmt.Sprintf("http=%d msg=%s", e.Status, e.Msg)
}

// asAPIError 是 errors.As 的轻量替代（只为这一处断言不值得多引一个包）。
func asAPIError(err error, target **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func isHTTPStatus(err error, status int) bool {
	var ae *apiError
	if asAPIError(err, &ae) {
		return ae.Status == status
	}
	return false
}

// ---------- 账号信息 ----------

// Customer 是调用上游所需的全部账号信息。
//
// 与 config.Account 解耦：网关那边用 NewCustomer 转一层，provider 包不认识
// config 包（避免循环依赖）。
type Customer struct {
	UID   string
	Token string
	// Domain 是 X-Domain 头的值，来自登录时 /login/account 的返回。
	Domain string
	// EnterpriseID 非空时补 X-Enterprise-Id / X-Tenant-Id。
	EnterpriseID string
	// Global 为真表示国际版账号（workbuddy-ai）：所有 base 切 workbuddy.ai，
	// UA 平台段与 Accept-Language 也要跟着换，否则会被风控判非法请求。
	Global bool
	// DeviceToken 是登录时上游签发的设备令牌，有就带上。
	DeviceToken string
}

// NewCustomer 构造 Customer（网关侧唯一入口）。
func NewCustomer(uid, token, domain, enterpriseID, deviceToken string, global bool) Customer {
	return Customer{
		UID:          strings.TrimSpace(uid),
		Token:        token,
		Domain:       strings.TrimSpace(domain),
		EnterpriseID: strings.TrimSpace(enterpriseID),
		DeviceToken:  deviceToken,
		Global:       global,
	}
}

func (c Customer) growthBase() string {
	if c.Global {
		return baseGlobal
	}
	return baseGrowthCN
}

func (c Customer) billingBase() string {
	if c.Global {
		return baseGlobal
	}
	return baseBillingCN
}

func (c Customer) referer() string {
	if c.Global {
		return baseGlobal
	}
	return baseRefererCN
}

func (c Customer) userAgent() string {
	platform := "WorkBuddy"
	if c.Global {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + wbClientVersion + " " + platform + "/" + wbClientVersion + " CLI/" + wbCliVersion
}

func (c Customer) acceptLanguage() string {
	if c.Global {
		return "en-US"
	}
	return "zh-CN"
}

// stableID 按 uid + 用途派生稳定的设备/会话标识（36 hex）。
//
// 为什么不用随机数：X-Machine-ID 必须跨重启恒定，否则每个号的「设备指纹」
// 天天变，反而更像爬虫。wb2api 用 sha256("wb2a:"+purpose+":"+uid)，这里同构。
func (c Customer) stableID(purpose string) string {
	if c.UID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("allin:wb:" + purpose + ":" + c.UID))
	return hex.EncodeToString(sum[:18])
}

// setHeaders 套上 billing / growth 共用的一套头。
//
// 一个都不能少：X-CodeBuddy-Request 是风控闸门头；X-User-Id 缺了 /v2/report
// 会 200 但静默丢弃事件（「上报了但连登不涨」的经典坑）。
// withUA 为真时补浏览器指纹头（growth 域要，billing 域 wb2api 不设，照抄）。
func (c Customer) setHeaders(req *http.Request, withUA bool) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", c.acceptLanguage())
	if withUA {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", c.referer())
		req.Header.Set("Referer", c.referer()+"/")
		req.Header.Set("User-Agent", c.userAgent())
		// 官方客户端指纹。缺了这些上游会判「不是官方客户端发出的」，
		// 直接回 11128（unapproved channel）——换成官方 App 就能过，
		// 差别就在这里：App 带全这一套，网关之前一个都没带。
		req.Header.Set("X-Product", "SaaS")
		req.Header.Set("X-IDE-Type", "WorkBuddy")
		req.Header.Set("X-IDE-Name", "WorkBuddy")
		req.Header.Set("X-IDE-Version", "5.9.3")
		req.Header.Set("X-Agent-Intent", "craft")
		req.Header.Set("X-Agent-Purpose", "conversation")
		req.Header.Set("X-Agent-Type", "main")
		req.Header.Set("X-Agent-Runtime", "local-agent")
		req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
		// 模拟官方 SDK 的运行时标识（Node + anthropic-sdk 形态）
		req.Header.Set("X-Stainless-Retry-Count", "0")
		req.Header.Set("X-Stainless-Timeout", "1200")
		req.Header.Set("X-Stainless-Lang", "js")
		req.Header.Set("X-Stainless-Package-Version", "6.25.0")
		req.Header.Set("X-Stainless-OS", "Linux")
		req.Header.Set("X-Stainless-Arch", "arm64")
		req.Header.Set("X-Stainless-Runtime", "node")
		req.Header.Set("X-Stainless-Runtime-Version", "v22.21.1")
	}
	if c.UID != "" {
		req.Header.Set("X-User-Id", c.UID)
	}
	if c.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", c.EnterpriseID)
		req.Header.Set("X-Tenant-Id", c.EnterpriseID)
	} else if c.Global {
		// 国际版个人号显式声明无企业，缺了会被按可疑请求判。
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	domain := c.Domain
	if c.Global {
		domain = "www.workbuddy.ai"
	}
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	}
	if c.DeviceToken != "" {
		req.Header.Set("X-Device-Token", c.DeviceToken)
	}
	if id := c.stableID("machine"); id != "" {
		req.Header.Set("X-Machine-ID", id)
	}
	if id := c.stableID("session"); id != "" {
		req.Header.Set("X-Session-ID", id)
	}
}

// call 发一个请求并解信封，返回 data 段原文。
//
// 传输出错返回的是普通 error（不是 apiError），上层不该把它当「上游说不」。
func (c Customer) call(ctx context.Context, base, method, path string, body any, withUA bool) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, withUA)

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return nil, fmt.Errorf("读取响应失败: %w", rerr)
	}

	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	parseErr := json.Unmarshal(raw, &env)

	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(env.Msg)
		if msg == "" {
			msg = truncateStr(string(raw), 160)
		}
		return nil, &apiError{Status: resp.StatusCode, Code: env.Code, Msg: msg}
	}
	if parseErr != nil {
		return nil, fmt.Errorf("响应不是合法 JSON（HTTP %d）: %s", resp.StatusCode, truncateStr(string(raw), 120))
	}
	if env.Code != 0 {
		return nil, &apiError{Status: resp.StatusCode, Code: env.Code, Msg: env.Msg}
	}
	return env.Data, nil
}

func (c Customer) growth(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	return c.call(ctx, c.growthBase(), method, path, body, true)
}

func (c Customer) billing(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	return c.call(ctx, c.billingBase(), method, path, body, false)
}

// decodeData 把 data 段解到 v；data 为空/null/{} 直接返回 nil。
func decodeData(raw json.RawMessage, v any) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "{}" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// asMap 把 data 段转成 map，失败返回 nil。
func asMap(raw json.RawMessage) map[string]any {
	var m map[string]any
	if err := decodeData(raw, &m); err != nil {
		return nil
	}
	return m
}

// clientToken 生成幂等令牌（前端 randomUUID 同款语义）。
//
// 兑换与抽奖都必须带：上游用它去重，重复请求不会重复发奖/扣次数。
func clientToken() string {
	b := make([]byte, 16)
	seed := time.Now().UnixNano()
	for i := range b {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		b[i] = byte(seed >> 24)
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// isBusinessReject 判断错误消息里是否含任一关键词（不区分大小写）。
func isBusinessReject(err error, keywords ...string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, k := range keywords {
		if strings.Contains(msg, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// isBuddyTaskIncomplete 判定「领养门槛未达标」：HTTP 400 + first_buddy 关键词。
func isBuddyTaskIncomplete(err error) bool {
	var ae *apiError
	if !asAPIError(err, &ae) || ae.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(ae.Msg), buddyTaskIncompleteMarker)
}

// ---------- 响应结构 ----------

// StreakInfo 连登完整状态（GET /activity/growth/streak）。
type StreakInfo struct {
	Streak struct {
		Days              int    `json:"days"`
		MonthTotalDays    int    `json:"month_total_days"`
		NextTier          string `json:"next_tier"`
		NextTierRemaining int    `json:"next_tier_remaining"`
	} `json:"streak"`
	MakeupCards struct {
		Balance int `json:"balance"`
		Max     int `json:"max"`
	} `json:"makeup_cards"`
	RedemptionStatus struct {
		Tier7dStatus  string `json:"tier_7d_status"`
		Tier14dStatus string `json:"tier_14d_status"`
		Tier28dStatus string `json:"tier_28d_status"`
		RemainingDays int    `json:"remaining_days"`
		Tiers         []struct {
			Tier    string `json:"tier"`
			Days    int    `json:"days"`
			Credit  int    `json:"credit"`
			Energy  int    `json:"energy"`
			Cards   int    `json:"cards"`
			Chances int    `json:"chances"`
		} `json:"tiers"`
	} `json:"redemption_status"`
}

// Buddy 猫档案。nil 表示没有猫。
type Buddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// TravelState 猫猫旅行状态。
type TravelState struct {
	State             string `json:"state"`               // idle / traveling / arrived
	DailyLimitReached bool   `json:"daily_limit_reached"` // 今日已派出过
	RecordID          int64  `json:"record_id"`           // 在途/到站记录 id，claim 必带
	RewardCredit      int64  `json:"reward_credit"`       // 到站可领奖励
}

// LotterySummary 抽奖次数（GET /activity/growth/lottery/summary）。
type LotterySummary struct {
	Chances int `json:"chances"`
	Module  struct {
		Enabled bool `json:"enabled"`
	} `json:"module"`
}

// CompensationStatus 补偿活动状态。
//
// active=false 是「当前没有补偿活动」的正常 200 返回，不是错误。
type CompensationStatus struct {
	Active         bool   `json:"active"`
	Claimed        bool   `json:"claimed"`
	CreditNum      int64  `json:"credit_num"`
	ValidityPeriod int64  `json:"validity_period"`
	EndTime        string `json:"end_time"`
}

// Pending 是否存在「可领且未领」的补偿——领之前必须过这一关。
//
// 三者与门：active=false 是没活动；claimed=true 是已领（再调只会拿业务错误）；
// credit_num<=0 是活动在但没额度。三种都归为「无需领取」。
func (s CompensationStatus) Pending() bool {
	return s.Active && !s.Claimed && s.CreditNum > 0
}

// ---------- 只读任务 ----------

// GetBuddyInfo 查猫档案。返回 (nil, nil) 表示无猫。
func GetBuddyInfo(ctx context.Context, c Customer) (*Buddy, error) {
	data, err := c.growth(ctx, http.MethodGet, pathBuddyInfo, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := decodeData(data, &resp); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var b Buddy
	if err := json.Unmarshal(resp.Buddy, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// GetStreak 查连登完整状态。
func GetStreak(ctx context.Context, c Customer) (*StreakInfo, error) {
	data, err := c.growth(ctx, http.MethodGet, pathStreak, nil)
	if err != nil {
		return nil, err
	}
	out := &StreakInfo{}
	if err := decodeData(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetLottery 查抽奖次数。
func GetLottery(ctx context.Context, c Customer) (*LotterySummary, error) {
	data, err := c.growth(ctx, http.MethodGet, pathLotterySum, nil)
	if err != nil {
		return nil, err
	}
	out := &LotterySummary{}
	if err := decodeData(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetCompensationStatus 查补偿活动状态。
//
// 端点用 POST（空体 {}），这是实测口径，不是笔误。
func GetCompensationStatus(ctx context.Context, c Customer) (*CompensationStatus, error) {
	data, err := c.billing(ctx, http.MethodPost, pathCompensationStatus, map[string]any{})
	if err != nil {
		return nil, err
	}
	out := &CompensationStatus{}
	if err := decodeData(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetTravelState 查旅行状态。无猫时的返回没实测，调用方应先查 BuddyInfo。
func GetTravelState(ctx context.Context, c Customer) (*TravelState, error) {
	data, err := c.growth(ctx, http.MethodGet, pathTravelStat, nil)
	if err != nil {
		return nil, err
	}
	out := &TravelState{}
	if err := decodeData(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// heatmapYesterdayMissed 昨日是否漏签（heatmap cell score==0）。
func heatmapYesterdayMissed(ctx context.Context, c Customer) (bool, error) {
	yesterday := time.Now().In(cstZone).AddDate(0, 0, -1).Format("2006-01-02")
	data, err := c.growth(ctx, http.MethodGet, pathHeatmap, nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := decodeData(data, &resp); err != nil {
		return false, err
	}
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// ---------- 写任务 ----------

// ReportActivity 发一条 chat_request_send 活跃上报（POST /v2/report）。
//
// 这是整套任务里最关键的一步：一条上报同时点亮 growth 连登、并解锁
// first_buddy 领养任务（领养前置）。字段必须全（照客户端 chat_request_send
// 形状，勿用最小 3 字段，防上游后续加严）；userId 必填，缺了服务端 200 但
// 静默丢弃事件。
func ReportActivity(ctx context.Context, c Customer) Outcome {
	now := time.Now().UnixMilli()
	conv := fmt.Sprintf("allin-wb-%d", now)
	ev := map[string]any{
		"eventCode":             "chat_request_send",
		"timestamp":             now,
		"reportDelay":           0,
		"mode":                  "craft",
		"conversationId":        conv,
		"requestId":             conv,
		"inputLength":           12,
		"requestModelId":        "deepseek-v4-flash",
		"requestModelName":      "DeepSeek V4 Flash",
		"isPlan":                false,
		"isAutoExecuteTerminal": false,
		"isAutoModify":          false,
		"codebaseEnable":        false,
		"maxToken":              0,
		"maxSteps":              0,
		"temperature":           0,
		"maxRetries":            0,
		"mentionContexts":       []any{},
		"knowledgeId":           []any{},
		"knowledgeName":         []any{},
		"codebaseId":            "",
		"mentionContextCount":   0,
		"command":               "",
		"expertId":              "",
		"recommendId":           "",
		"skillId":               "",
		"skillCount":            0,
		"totalCount":            0,
		"fileUri":               "",
		"presentAt":             now,
		"traceId":               "",
		"rootRequestId":         conv,
		"parentConversationId":  conv,
		"agentName":             "default",
		"agentType":             "conversation",
		"userId":                c.UID,
	}
	// 上游收的是数组（客户端一个 batch 推事件）。
	body := []map[string]any{ev}
	if _, err := c.billing(ctx, http.MethodPost, pathReport, body); err != nil {
		return failed(err)
	}
	return done(OutcomeOK, "活跃已上报（点亮连登 + 解锁 first_buddy）")
}

// ClaimGift 领新手礼包（每号一次，已领返回业务错误）。
func ClaimGift(ctx context.Context, c Customer) Outcome {
	data, err := c.billing(ctx, http.MethodPost, pathClaimGift, map[string]any{})
	if err != nil {
		switch {
		case isBusinessReject(err, "已领取", "已领过", "already", "重复"):
			return done(OutcomeAlready, err.Error())
		case isBusinessReject(err, "不支持", "not support", "企业"):
			return done(OutcomeSkipped, "当前账号不支持该活动: "+err.Error())
		}
		return failed(err)
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = decodeData(data, &resp)
	out := done(OutcomeOK, "礼包已领取")
	out.Credit = resp.Credit
	return out
}

// ClaimCompensation 先查状态再领补偿。
//
// 顺序是硬的：直接盲领会把「没有补偿」「已经领过」「领取真失败」压成同一形态，
// 而补偿带截止日，静默过期后用户全程无感知。所以先过 Pending() 判据再发请求。
func ClaimCompensation(ctx context.Context, c Customer) Outcome {
	st, err := GetCompensationStatus(ctx, c)
	if err != nil {
		out := failed(err)
		out.Message = "查补偿状态失败: " + err.Error()
		return out
	}
	if !st.Pending() {
		switch {
		case !st.Active:
			return done(OutcomeSkipped, "当前无生效补偿活动")
		case st.Claimed:
			return done(OutcomeAlready, "本次活动已领取（截止 "+st.EndTime+"）")
		default:
			return done(OutcomeSkipped, "活动在但额度为 0")
		}
	}
	data, err := c.billing(ctx, http.MethodPost, pathClaimCompensation, map[string]any{})
	if err != nil {
		out := failed(err)
		out.Message = fmt.Sprintf("待领 %d 积分但领取失败: %v", st.CreditNum, err)
		return out
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = decodeData(data, &resp)
	if resp.Credit == 0 {
		// 上游偶尔不回 credit，用查到的额度兜底，免得报表显示「领了 0 分」。
		resp.Credit = st.CreditNum
	}
	out := done(OutcomeOK, "补偿已领取（截止 "+st.EndTime+"）")
	out.Credit = resp.Credit
	return out
}

// UseMakeupCard 用补签卡补指定日期（保连登连续天数）。
func UseMakeupCard(ctx context.Context, c Customer, date string) Outcome {
	_, err := c.growth(ctx, http.MethodPost, pathMakeupUse, map[string]any{"target_date": date})
	if err != nil {
		if isBusinessReject(err, "补签卡", "no card", "用完") {
			return done(OutcomeSkipped, "没有可用补签卡: "+err.Error())
		}
		return failed(err)
	}
	return done(OutcomeOK, "已用补签卡补签 "+date)
}

// RunMakeup 昨日漏签且有卡时自动补签。
func RunMakeup(ctx context.Context, c Customer) Outcome {
	missed, err := heatmapYesterdayMissed(ctx, c)
	if err != nil {
		return failed(err)
	}
	if !missed {
		return done(OutcomeSkipped, "昨日未漏签，无需补签")
	}
	full, err := GetStreak(ctx, c)
	if err != nil {
		return failed(err)
	}
	if full.MakeupCards.Balance <= 0 {
		return done(OutcomeSkipped, "昨日漏签但没有补签卡")
	}
	yesterday := time.Now().In(cstZone).AddDate(0, 0, -1).Format("2006-01-02")
	return UseMakeupCard(ctx, c, yesterday)
}

// RunStreak 连登档位兑换：7d/14d/28d 到了就兑，兑出发积分/补签卡/抽奖次数。
//
// 未解锁时上游 403「连续登录天数不足」，属预期（按钮恒可点，服务端把关）。
func RunStreak(ctx context.Context, c Customer) Outcome {
	full, err := GetStreak(ctx, c)
	if err != nil {
		return failed(err)
	}
	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	var redeemed []string
	var credit int64
	for _, tier := range full.RedemptionStatus.Tiers {
		if s := statuses[tier.Tier]; s == "locked" || s == "claimed" {
			continue
		}
		_, err := c.growth(ctx, http.MethodPost, pathRedeem,
			map[string]any{"tier": tier.Tier, "client_token": clientToken()})
		if err != nil {
			// 403 = 天数不足，预期内；其它错误也继续看下一档，别让一档拖死全部。
			continue
		}
		redeemed = append(redeemed, tier.Tier)
		credit += int64(tier.Credit)
	}
	out := done(OutcomeOK, fmt.Sprintf("连登 %d 天", full.Streak.Days))
	out.Credit = credit
	switch {
	case len(redeemed) > 0:
		out.Message = fmt.Sprintf("连登 %d 天，本次兑换 %s 档", full.Streak.Days, strings.Join(redeemed, "/"))
	case full.Streak.Days < 7:
		out.Status = OutcomeLocked
		out.Message = fmt.Sprintf("连登 %d 天，还差 %d 天到 7 天档",
			full.Streak.Days, full.RedemptionStatus.RemainingDays)
	default:
		out.Status = OutcomeAlready
		out.Message = fmt.Sprintf("连登 %d 天，档位均已兑换", full.Streak.Days)
	}
	return out
}

// RunLottery 按当前次数把抽奖抽完。
func RunLottery(ctx context.Context, c Customer) Outcome {
	sum, err := GetLottery(ctx, c)
	if err != nil {
		return failed(err)
	}
	if sum.Chances <= 0 {
		return done(OutcomeSkipped, "没有抽奖次数")
	}
	var prizes []any
	drawed := 0
	for i := 0; i < sum.Chances; i++ {
		raw, err := c.growth(ctx, http.MethodPost, pathLotteryDraw,
			map[string]any{"client_token": clientToken()})
		if err != nil {
			if drawed == 0 {
				return failed(err)
			}
			return Outcome{
				Status:  OutcomeOK,
				Message: fmt.Sprintf("抽了 %d/%d 次后中断: %v", drawed, sum.Chances, err),
				Data:    map[string]any{"draws": drawed, "prizes": prizes},
			}
		}
		drawed++
		if m := asMap(raw); m != nil {
			prizes = append(prizes, m)
		}
	}
	return Outcome{
		Status:  OutcomeOK,
		Message: fmt.Sprintf("抽奖 %d 次", drawed),
		Data:    map[string]any{"draws": drawed, "prizes": prizes},
	}
}

// BuddyFirst 同意协议 + 领养第一只猫。
//
// 前置：必须先跑 ReportActivity，否则返回 400 "first_buddy task not completed yet"
// ——那道门槛的真实来源是「当日无活跃上报」，不是账号问题。
func BuddyFirst(ctx context.Context, c Customer) Outcome {
	// 协议幂等，失败不阻塞，让领养按既有错误路径暴露。
	_, _ = c.growth(ctx, http.MethodPost, pathBuddyAgree, map[string]any{"agree": true})
	_, err := c.growth(ctx, http.MethodPost, pathBuddyFirst, map[string]any{})
	if err != nil {
		switch {
		case isBuddyTaskIncomplete(err):
			return done(OutcomeSkipped, "对话活跃门槛未达标，领养留到明天再试")
		case isBusinessReject(err, "已领养", "already"):
			return done(OutcomeAlready, "已经领养过了")
		}
		return failed(err)
	}
	return done(OutcomeOK, "领养成功（+300 积分）")
}

// RunTravelDepart 派出猫旅行（每日 1 次，自然日 00:00 CST 重置）。
func RunTravelDepart(ctx context.Context, c Customer) Outcome {
	st, err := GetTravelState(ctx, c)
	if err != nil {
		return failed(err)
	}
	switch st.State {
	case "arrived":
		return done(OutcomeSkipped, fmt.Sprintf("猫已到站，先去领奖（record=%d）", st.RecordID))
	case "traveling":
		return done(OutcomeAlready, fmt.Sprintf("猫在途中（record=%d）", st.RecordID))
	}
	if st.DailyLimitReached {
		return done(OutcomeAlready, "今日已派出过（自然日 00:00 CST 重置）")
	}
	_, err = c.growth(ctx, http.MethodPost, pathTravelGo, map[string]any{"location_id": travelLocationID})
	if err != nil {
		if isBusinessReject(err, "上限", "limit", "已派出", "已达") {
			return done(OutcomeAlready, err.Error())
		}
		return failed(err)
	}
	return done(OutcomeOK, fmt.Sprintf("已派出（地点 %d）", travelLocationID))
}

// RunTravelClaim 领取到站奖励（必须带 record_id，从 status 取）。
func RunTravelClaim(ctx context.Context, c Customer) Outcome {
	st, err := GetTravelState(ctx, c)
	if err != nil {
		return failed(err)
	}
	switch st.State {
	case "traveling":
		return done(OutcomeSkipped, fmt.Sprintf("猫还在途中（record=%d），到站后才能领", st.RecordID))
	case "arrived":
		// 继续往下领
	default:
		return done(OutcomeSkipped, "猫在家（idle），没有可领的旅行奖励")
	}
	if st.RecordID == 0 {
		return done(OutcomeSkipped, "到站但上游没给 record_id，跳过")
	}
	data, err := c.growth(ctx, http.MethodPost, pathTravelClaim, map[string]any{"record_id": st.RecordID})
	if err != nil {
		if isBusinessReject(err, "已领", "already") {
			return done(OutcomeAlready, err.Error())
		}
		return failed(err)
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	_ = decodeData(data, &resp)
	if resp.RewardCredit == 0 {
		resp.RewardCredit = st.RewardCredit
	}
	out := done(OutcomeOK, fmt.Sprintf("旅行奖励已领（record=%d）", st.RecordID))
	out.Credit = resp.RewardCredit
	return out
}

// ---------- 全套编排 ----------

// TaskResult 是 RunAllTasks 返回的单任务结果。
type TaskResult struct {
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"`
	Err     string  `json:"error,omitempty"`
}

// TaskOptions 控制全套跑的开关。零值 = 按默认流程全跑。
type TaskOptions struct {
	// SkipReport 跳过活跃上报。只在调试时用：跳过会导致连登不点亮、
	// 领养门槛过不去。
	SkipReport bool
	// SkipCheckin 跳过签到步骤。
	SkipCheckin bool
	// Checkin 是外部注入的签到函数，nil 表示不跑签到。
	// 这样 tasks.go 不必了解签到那套既有逻辑（它在 gateway 里）。
	Checkin func(ctx context.Context) Outcome
	// TravelWait 派出后到领奖之间的等待。上游到站要时间，本次派出当天
	// 大概率还是 traveling，等太久没意义，默认 0（只查一次状态）。
	TravelWait time.Duration
	// QuestGap 各步骤之间的间隔，防风控。零值用 800ms。
	QuestGap time.Duration
}

// Gap 是带 ctx 感知的等待：ctx 取消立刻返回，别把超时请求拖成两分钟。
func Gap(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// RunAllTasks 一次性跑全套任务，返回逐项结果。
//
// 顺序不是随便排的，每一步都有理由：
//
//  1. report       先点亮连登 + 解锁 first_buddy，后面 growth 任务吃这个前置
//  2. checkin      签到（由调用方注入的既有实现）
//  3. streak       兑换档位发抽奖次数，必须在 lottery 之前
//  4. claim-gift   领礼包，独立任务
//  5. compensation 先查状态再领，独立任务
//  6. makeup       补签卡补昨天，放在 streak 之后，避免「刚补完就兑换」的档位抖动
//  7. buddy        查猫档案；无猫走领养（agreement + first），有猫才进旅行
//  8. travel-depart 派出
//  9. travel-claim  查状态，到站才领
//  10. lottery     最后抽，此时次数已由第 3 步累加
func RunAllTasks(ctx context.Context, c Customer, opts TaskOptions) []TaskResult {
	if opts.QuestGap <= 0 {
		opts.QuestGap = 800 * time.Millisecond
	}
	var results []TaskResult
	record := func(name string, out Outcome) {
		r := TaskResult{Name: name, Outcome: out}
		if out.Status == OutcomeFailed {
			r.Err = out.Message
		}
		results = append(results, r)
	}

	if !opts.SkipReport {
		record("report", ReportActivity(ctx, c))
		Gap(ctx, opts.QuestGap)
	}

	if !opts.SkipCheckin && opts.Checkin != nil {
		record("checkin", opts.Checkin(ctx))
		Gap(ctx, opts.QuestGap)
	}

	record("streak", RunStreak(ctx, c))
	Gap(ctx, opts.QuestGap)

	record("claim-gift", ClaimGift(ctx, c))
	Gap(ctx, opts.QuestGap)

	record("compensation", ClaimCompensation(ctx, c))
	Gap(ctx, opts.QuestGap)

	record("makeup", RunMakeup(ctx, c))
	Gap(ctx, opts.QuestGap)

	buddy, err := GetBuddyInfo(ctx, c)
	switch {
	case err != nil:
		record("buddy", failed(err))
		record("travel-depart", done(OutcomeSkipped, "猫档案查不到，跳过旅行"))
		record("travel-claim", done(OutcomeSkipped, "猫档案查不到，跳过领奖"))
	case buddy == nil:
		record("buddy", BuddyFirst(ctx, c))
		record("travel-depart", done(OutcomeSkipped, "刚领养，明天再出发"))
		record("travel-claim", done(OutcomeSkipped, "刚领养，没有可领奖励"))
	default:
		record("buddy", Outcome{Status: OutcomeOK, Message: "已有猫：" + buddy.Name})
		Gap(ctx, opts.QuestGap)
		record("travel-depart", RunTravelDepart(ctx, c))
		Gap(ctx, opts.QuestGap)
		Gap(ctx, opts.TravelWait)
		record("travel-claim", RunTravelClaim(ctx, c))
	}
	Gap(ctx, opts.QuestGap)

	record("lottery", RunLottery(ctx, c))

	return results
}

// StatusSnapshot 是只读状态汇总（GET /api/workbuddy/tasks/status 用的）。
type StatusSnapshot struct {
	Buddy        *Buddy              `json:"buddy,omitempty"`
	BuddyError   string              `json:"buddyError,omitempty"`
	Streak       *StreakInfo         `json:"streak,omitempty"`
	StreakError  string              `json:"streakError,omitempty"`
	Lottery      *LotterySummary     `json:"lottery,omitempty"`
	LotteryError string              `json:"lotteryError,omitempty"`
	Compensation *CompensationStatus `json:"compensation,omitempty"`
	CompError    string              `json:"compensationError,omitempty"`
	Travel       *TravelState        `json:"travel,omitempty"`
	TravelError  string              `json:"travelError,omitempty"`
}

// GetStatus 串行拉一遍只读状态。串行而非并发：同一账号四条在途请求更容易
// 被风控盯上，而这点延迟对人工查询无所谓。单个失败不影响其余。
func GetStatus(ctx context.Context, c Customer, gapDur time.Duration) StatusSnapshot {
	var snap StatusSnapshot

	if b, err := GetBuddyInfo(ctx, c); err != nil {
		snap.BuddyError = err.Error()
	} else {
		snap.Buddy = b
	}
	Gap(ctx, gapDur)

	if s, err := GetStreak(ctx, c); err != nil {
		snap.StreakError = err.Error()
	} else {
		snap.Streak = s
	}
	Gap(ctx, gapDur)

	if l, err := GetLottery(ctx, c); err != nil {
		snap.LotteryError = err.Error()
	} else {
		snap.Lottery = l
	}
	Gap(ctx, gapDur)

	if comp, err := GetCompensationStatus(ctx, c); err != nil {
		snap.CompError = err.Error()
	} else {
		snap.Compensation = comp
	}

	// 只有有猫才查旅行状态（无猫时这个端点的返回没实测过，不瞎打）。
	if snap.Buddy != nil {
		Gap(ctx, gapDur)
		if ts, err := GetTravelState(ctx, c); err != nil {
			snap.TravelError = err.Error()
		} else {
			snap.Travel = ts
		}
	}
	return snap
}
