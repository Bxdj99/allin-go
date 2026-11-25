package provider

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// WorkBuddy 429 的两种成因，必须分开处理。
//
// 实测（2026-11-25，gateway.log 里 451 次 429）全部是这一种：
//
//	{"error":{"data":{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包…"}}}
//
// 额度耗尽**不会自己恢复**，把它当「限流」每 2 分钟重试一次毫无意义 ——
// 这正是那 451 次刷屏的来源，每次都在烧一次上游往返。
//
// 另一种是真正的频率限制，上游会给出明确的重置时刻：
//
//	您的使用量已超出频率限制，将在 2026-11-26 08:00:00 UTC+8 恢复
//	usage exceeds frequency limit, but don't worry, your usage will reset at ...
//
// 这种情况应当冷却到那个时刻，而不是固定 2 分钟。
//
// 判定顺序参考上游 WorkBuddyQuotaCooldown.java:32-78：
// 先认「额度耗尽」并直接排除，再解析重置时刻。

// creditExhaustedCodes 是表示「额度已用尽」的业务码。
// 14018 是 WorkBuddy 的固定码；credits_exhausted 是字符串形态。
var creditExhaustedCodes = []string{"14018", "credits_exhausted", "11140"}

// quotaResetPattern 匹配上游给出的窗口重置时刻。
// 允许 UTC/GMT 与时区偏移，中英文各一套文案。
var quotaResetPattern = regexp.MustCompile(`(?i)(?:您的使用量已超出频率限制[，,]\s*将在\s*` +
	`|usage exceeds frequency limit,\s*but don['’]t worry,\s*your usage will reset at\s+)` +
	`(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})\s*` +
	`(?:UTC|GMT)([+-])(\d{1,2})(?::?(\d{2}))?`)

// maxQuotaWindow 是可信窗口的上限。超过这个长度的「重置时刻」多半是解析错了，
// 宁可退回短冷却，也不要让账号被误冻一天以上。
const maxQuotaWindow = 24*time.Hour + 5*time.Minute

// IsCreditExhausted 判断一次 429 响应体是不是「额度已用尽」。
//
// 用尽与限流要分开：用尽的账号短期内不会再可用，
// 限流则应冷却到上游给出的重置时刻。
func IsCreditExhausted(errBody string) bool {
	if errBody == "" || len(errBody) > 64<<10 {
		return false
	}
	var probe struct {
		Code  any `json:"code"`
		Error struct {
			Code any `json:"code"`
			Data struct {
				Code any `json:"code"`
			} `json:"data"`
			Message string `json:"message"`
		} `json:"error"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(errBody), &probe) != nil {
		return false
	}

	candidates := []string{
		asString(probe.Code),
		asString(probe.Error.Code),
		asString(probe.Error.Data.Code),
	}
	for _, c := range candidates {
		for _, want := range creditExhaustedCodes {
			if c == want {
				return true
			}
		}
	}

	// 有些返回只给文案不给码。
	for _, msg := range []string{probe.Error.Message, probe.Msg, probe.Message} {
		if strings.Contains(msg, "额度已用尽") || strings.Contains(strings.ToLower(msg), "credits_exhausted") {
			return true
		}
	}
	return false
}

// asString 把 JSON 里的数字/字符串统一成字符串。
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// 14018 这种整数不能被格式化成 14018.000000
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	return ""
}

// QuotaWindowResetAt 解析 429 响应体里给出的窗口重置时刻。
//
// 返回零值表示「不是可识别的窗口限流」——调用方应退回较短的默认冷却。
// 额度已用尽的情况显式返回零值：那不是窗口，等窗口没用。
func QuotaWindowResetAt(errBody string, now time.Time) time.Time {
	if errBody == "" || len(errBody) > 64<<10 {
		return time.Time{}
	}
	if IsCreditExhausted(errBody) {
		return time.Time{}
	}

	msg := extractMessage(errBody)
	if msg == "" {
		return time.Time{}
	}
	m := quotaResetPattern.FindStringSubmatch(msg)
	if m == nil {
		return time.Time{}
	}

	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	hour, _ := strconv.Atoi(m[4])
	min, _ := strconv.Atoi(m[5])
	sec, _ := strconv.Atoi(m[6])
	sign := 1
	if m[7] == "-" {
		sign = -1
	}
	offH, _ := strconv.Atoi(m[8])
	offM := 0
	if m[9] != "" {
		offM, _ = strconv.Atoi(m[9])
	}
	if month < 1 || month > 12 || offH > 14 || offM > 59 {
		return time.Time{}
	}

	// 上游的「UTC+8」表示该时刻以东八区显示。
	offset := sign * (offH*3600 + offM*60)
	loc := time.FixedZone("upstream", offset)
	reset := time.Date(year, time.Month(month), day, hour, min, sec, 0, loc)

	if !reset.After(now) {
		return time.Time{}
	}
	if reset.Sub(now) > maxQuotaWindow {
		// 解析出离谱的远未来，多半是文案格式变了，别锁死账号
		return time.Time{}
	}
	return reset
}

// extractMessage 从各种 429 响应体形态里挖出人读的错误文案。
func extractMessage(errBody string) string {
	var probe struct {
		Error   json.RawMessage `json:"error"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
	}
	if json.Unmarshal([]byte(errBody), &probe) != nil {
		return ""
	}
	// error 有时是字符串，有时是对象
	if len(probe.Error) > 0 {
		var s string
		if json.Unmarshal(probe.Error, &s) == nil && s != "" {
			return s
		}
		var obj struct {
			Message string `json:"message"`
			Data    struct {
				Msg string `json:"msg"`
			} `json:"data"`
		}
		if json.Unmarshal(probe.Error, &obj) == nil {
			if obj.Message != "" {
				return obj.Message
			}
			if obj.Data.Msg != "" {
				return obj.Data.Msg
			}
		}
	}
	if probe.Msg != "" {
		return probe.Msg
	}
	return probe.Message
}
