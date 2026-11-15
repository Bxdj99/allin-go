package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// 积分（额度）查询。
//
// 上游：POST {billingBase}/billing/meter/get-user-resource
// 返回是**四层嵌套**：
//
//	{"code":0,"data":{"Response":{"Data":{"TotalCount":52,"TotalDosage":996,
//	    "Accounts":[ {"PackageName":"...","CapacityUnit":"credits",
//	                  "CapacitySize":100,"CapacityUsed":100,"CapacityRemain":0,
//	                  "CycleCapacitySize":100,"CycleCapacityRemain":0,
//	                  "ExpiredTime":"...","Status":..,"CapacityType":1}, ... ]}}}}
//
// 单位就是 credits，不做换算。
// 一个账号可能有几十个资源包（52 条那种），要按「未过期 + 有剩余」过滤后汇总。

const pathUserResource = "/billing/meter/get-user-resource"

// CreditBucket 是一个资源包的额度明细。
type CreditBucket struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind,omitempty"`
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Percent   float64 `json:"percent"`
	ExpireAt  string  `json:"expireAt,omitempty"`
	Status    int     `json:"status,omitempty"`
}

// CreditSummary 是一个账号的积分总览。
type CreditSummary struct {
	AccountID  string         `json:"accountId"`
	Total      float64        `json:"total"`
	Used       float64        `json:"used"`
	Remaining  float64        `json:"remaining"`
	Percent    float64        `json:"percent"`
	Packages   int            `json:"packages"`        // 资源包总数
	Active     int            `json:"activePackages"`  // 有剩余的包数
	Buckets    []CreditBucket `json:"buckets,omitempty"`
	ExpireAt   string         `json:"expireAt,omitempty"` // 最早到期的有余额包
	Note       string         `json:"note,omitempty"`
	ObservedAt int64          `json:"observedAt"`
}

// creditAccount 是上游 Accounts 数组里的一条资源包记录。
type creditAccount struct {
	PackageName        string      `json:"PackageName"`
	ProductName        string      `json:"ProductName"`
	CapacityUnit       string      `json:"CapacityUnit"`
	CapacityType       int         `json:"CapacityType"`
	CapacitySize       json.Number `json:"CapacitySize"`
	CapacityUsed       json.Number `json:"CapacityUsed"`
	CapacityRemain     json.Number `json:"CapacityRemain"`
	CycleCapacitySize  json.Number `json:"CycleCapacitySize"`
	CycleCapacityUsed  json.Number `json:"CycleCapacityUsed"`
	CycleCapacityRem  json.Number `json:"CycleCapacityRemain"`
	ExpiredTime        string      `json:"ExpiredTime"`
	CycleEndTime       string      `json:"CycleEndTime"`
	Status             int         `json:"Status"`
}

// GetCredits 查询单个账号的积分。
func GetCredits(ctx context.Context, c Customer) (*CreditSummary, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.AddDate(1, 0, 0).Format("2006-01-02 15:04:05"),
	}

	raw, err := c.billing(ctx, "POST", pathUserResource, body)
	if err != nil {
		return nil, err
	}

	// billing() 剥掉了外层信封，这里拿到的是 data
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

	// 按包汇总：只统计「有剩余」的包，已用尽的不计入剩余但计入总量
	var earliestExpire time.Time
	earliestName := ""
	for _, a := range accounts {
		size := numF(a.CapacitySize)
		used := numF(a.CapacityUsed)
		remain := numF(a.CapacityRemain)

		// 有些包只在 Cycle* 字段上有值
		if size == 0 {
			size = numF(a.CycleCapacitySize)
		}
		if remain == 0 {
			remain = numF(a.CycleCapacityRem)
		}
		if used == 0 {
			used = numF(a.CycleCapacityUsed)
		}
		if size == 0 && remain == 0 {
			continue // 完全没数据的包跳过
		}

		sum.Total += size
		sum.Used += used
		sum.Remaining += remain

		if remain > 0 {
			sum.Active++
			b := CreditBucket{
				Name:  pickName(a),
				Total: round2(size), Used: round2(used), Remaining: round2(remain),
			}
			if size > 0 {
				b.Percent = round2(remain / size * 100)
			}
			if a.ExpiredTime != "" {
				b.ExpireAt = a.ExpiredTime
				// 解析到期时间，找最早的那个
				if t, err := time.ParseInLocation("2006-01-02 15:04:05", a.ExpiredTime, time.Local); err == nil {
					if earliestExpire.IsZero() || t.Before(earliestExpire) {
						earliestExpire = t
						earliestName = b.Name
					}
				}
			} else if a.CycleEndTime != "" {
				b.ExpireAt = a.CycleEndTime
			}
			sum.Buckets = append(sum.Buckets, b)
		}
	}

	if sum.Total > 0 {
		sum.Percent = round2(sum.Remaining / sum.Total * 100)
	}
	if !earliestExpire.IsZero() {
		sum.ExpireAt = earliestExpire.Format("2006-01-02 15:04") + "（" + earliestName + "）"
	}
	if sum.Active == 0 {
		sum.Note = "所有资源包额度已用尽"
	}
	return sum, nil
}

func pickName(a creditAccount) string {
	if a.PackageName != "" {
		return a.PackageName
	}
	if a.ProductName != "" {
		return a.ProductName
	}
	return "额度包"
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
