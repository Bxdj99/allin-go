package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 积分（额度）查询。
//
// 上游：POST {billingBase}/billing/meter/get-user-resource
// 返回四层嵌套：
//
//	{"code":0,"data":{"Response":{"Data":{"TotalCount":52,"TotalDosage":737,
//	    "Accounts":[{...}]}}}}
//
// 每条 Account 是一个「资源包」，关键字段：
//
//	CapacitySize / CapacityUsed / CapacityRemain           整数口径（会抹零）
//	CapacitySizePrecise / ...Precise / ...Precise          精确口径（带小数）
//	CycleStartTime / CycleEndTime                          本周期起止（月度重置）
//	Status                                                 0=有效 3=已耗尽
//	SubProductCode                                         sp_tcaca_codebuddyide_bonus_pack=赠送包
//	AccountAttributes                                      grantSource: growth_travel/growth_checkin…
//
// 口径说明（用户反馈「积分不准」后核对）：
//   - 必须用 Precisе 字段，整数口径会丢小数（实测 1.6 被抹成 1）
//   - 「剩余」要按 Status 分组看：有效包合计才是真能用掉的量
//   - 百分比分母用「有效包总量」，不是历史累计发放量

const pathUserResource = "/billing/meter/get-user-resource"

// CreditBucket 是一个资源包的额度明细。
type CreditBucket struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind,omitempty"`  // gift / plan / extra
	Source    string  `json:"source,omitempty"` // grantSource：growth_travel / growth_checkin…
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Percent   float64 `json:"percent"`
	CycleEnd  string  `json:"cycleEnd,omitempty"` // 本周期结束（月度重置点）
	Status    int     `json:"status,omitempty"`
}

// CreditSummary 是一个账号的积分总览。
type CreditSummary struct {
	AccountID string `json:"accountId"`

	// 当前可用（只算 Status=0 的有效包）
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Percent   float64 `json:"percent"`

	// 历史累计（含已耗尽的包），用于说明「发过多少」
	LifetimeSize float64 `json:"lifetimeSize"`

	Packages  int `json:"packages"`       // 资源包总数
	Active    int `json:"activePackages"` // 还有余额的包数
	Exhausted int `json:"exhaustedPackages"`

	Buckets []CreditBucket `json:"buckets,omitempty"`

	// ResetAt 是最近一次额度重置点（有效包里最早的 CycleEndTime）
	ResetAt string `json:"resetAt,omitempty"`
	// ExpiringSoon 是 7 天内到期且有余额的积分
	ExpiringSoon float64 `json:"expiringSoon,omitempty"`

	Note       string `json:"note,omitempty"`
	ObservedAt int64  `json:"observedAt"`
}

// creditAccount 是上游 Accounts 数组里的一条资源包记录。
type creditAccount struct {
	PackageName       string      `json:"PackageName"`
	ProductName       string      `json:"ProductName"`
	SubProductCode    string      `json:"SubProductCode"`
	CapacityUnit      string      `json:"CapacityUnit"`
	CapacityType      int         `json:"CapacityType"`
	CapacitySize      json.Number `json:"CapacitySize"`
	CapacityUsed      json.Number `json:"CapacityUsed"`
	CapacityRemain    json.Number `json:"CapacityRemain"`
	SizePrecise       json.Number `json:"CapacitySizePrecise"`
	UsedPrecise       json.Number `json:"CapacityUsedPrecise"`
	RemainPrecise     json.Number `json:"CapacityRemainPrecise"`
	CycleSizePrecise  json.Number `json:"CycleCapacitySizePrecise"`
	CycleUsedPrecise  json.Number `json:"CycleCapacityUsedPrecise"`
	CycleRemainPrec   json.Number `json:"CycleCapacityRemainPrecise"`
	CycleStartTime    string      `json:"CycleStartTime"`
	CycleEndTime      string      `json:"CycleEndTime"`
	ExpiredTime       string      `json:"ExpiredTime"`
	Status            int         `json:"Status"`
	AccountAttributes []struct {
		Key   string `json:"Key"`
		Value string `json:"Value"`
		Type  int    `json:"Type"`
	} `json:"AccountAttributes"`
}

// grantSource 从 AccountAttributes 里取发放来源。
func (a creditAccount) grantSource() string {
	for _, at := range a.AccountAttributes {
		if strings.EqualFold(at.Key, "grantSource") {
			return at.Value
		}
	}
	return ""
}

// GetCredits 查询单个账号的积分。
func GetCredits(ctx context.Context, c Customer) (*CreditSummary, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 200,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.AddDate(1, 0, 0).Format("2006-01-02 15:04:05"),
	}

	raw, err := c.billing(ctx, "POST", pathUserResource, body)
	if err != nil {
		return nil, err
	}

	var outer struct {
		Response struct {
			Data struct {
				TotalCount  int             `json:"TotalCount"`
				TotalDosage json.Number     `json:"TotalDosage"`
				Accounts    []creditAccount `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, fmt.Errorf("额度数据解析失败: %w", err)
	}
	accounts := outer.Response.Data.Accounts

	sum := &CreditSummary{
		AccountID:  c.UID,
		Packages:   len(accounts),
		ObservedAt: time.Now().Unix(),
	}
	if len(accounts) == 0 {
		sum.Note = "上游没有返回资源包（该账号可能未开通计费）"
		return sum, nil
	}

	var soonestReset time.Time
	soonThreshold := now.AddDate(0, 0, 7)

	for _, a := range accounts {
		size, used, remain := a.precise()

		// 历史累计：所有包都算（说明「一共发过多少」）
		sum.LifetimeSize += size

		if a.Status != 0 || remain <= 0 {
			// 已耗尽（Status=3）或没余额：不计入当前可用
			if a.Status != 0 {
				sum.Exhausted++
			}
			continue
		}

		sum.Active++
		sum.Total += size
		sum.Used += used
		sum.Remaining += remain

		b := CreditBucket{
			Name: pickName(a), Kind: classifyPackage(a), Source: a.grantSource(),
			Total: round2(size), Used: round2(used), Remaining: round2(remain),
			CycleEnd: a.displayEnd(), Status: a.Status,
		}
		if size > 0 {
			b.Percent = round2(remain / size * 100)
		}
		sum.Buckets = append(sum.Buckets, b)

		// 重置时间 & 临期
		if t, ok := parseUpstreamTime(a.displayEnd()); ok {
			if soonestReset.IsZero() || t.Before(soonestReset) {
				soonestReset = t
			}
			if t.Before(soonThreshold) {
				sum.ExpiringSoon += remain
			}
		}
	}

	if sum.Total > 0 {
		sum.Percent = round2(sum.Remaining / sum.Total * 100)
	}
	if !soonestReset.IsZero() {
		sum.ResetAt = soonestReset.Format("2006-01-02 15:04")
	}

	// 按剩余从多到少排，最值钱的包排前面
	sort.SliceStable(sum.Buckets, func(i, j int) bool {
		return sum.Buckets[i].Remaining > sum.Buckets[j].Remaining
	})

	switch {
	case sum.Active == 0:
		sum.Note = "所有资源包额度已用尽"
	case sum.Total == 0:
		sum.Note = "有效包总量为 0，无法计算百分比"
	}
	return sum, nil
}

// precise 返回精确口径的 size/used/remain，缺失时退回整数口径。
func (a creditAccount) precise() (size, used, remain float64) {
	pick := func(prec, plain json.Number) float64 {
		if v := numF(prec); v > 0 {
			return v
		}
		return numF(plain)
	}
	size = pick(a.SizePrecise, a.CapacitySize)
	if size == 0 {
		size = pick(a.CycleSizePrecise, a.CapacitySize)
	}
	used = pick(a.UsedPrecise, a.CapacityUsed)
	remain = pick(a.RemainPrecise, a.CapacityRemain)
	if remain == 0 && size > 0 && used > 0 && used < size {
		remain = size - used
	}
	return
}

// displayEnd 返回该包的到期/重置时间字符串。
func (a creditAccount) displayEnd() string {
	if a.CycleEndTime != "" {
		return a.CycleEndTime
	}
	return a.ExpiredTime
}

func pickName(a creditAccount) string {
	if a.SubProductCode != "" {
		switch {
		case strings.Contains(a.SubProductCode, "bonus_pack"):
			return "赠送包"
		case strings.Contains(a.SubProductCode, "plan"):
			return "套餐额度"
		}
	}
	if a.PackageName != "" {
		return a.PackageName
	}
	if a.ProductName != "" {
		return a.ProductName
	}
	return "额度包"
}

// classifyPackage 粗分类型，面板上做不同颜色。
func classifyPackage(a creditAccount) string {
	switch {
	case strings.Contains(a.SubProductCode, "bonus_pack"):
		return "gift"
	case strings.Contains(a.SubProductCode, "plan"):
		return "plan"
	}
	return "extra"
}

// parseUpstreamTime 解析上游的墙钟时间（UTC+8）。
func parseUpstreamTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	loc := time.FixedZone("CST", 8*3600)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func numF(n json.Number) float64 {
	if n == "" {
		return 0
	}
	f, err := n.Float64()
	if err != nil {
		return 0
	}
	return f
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
