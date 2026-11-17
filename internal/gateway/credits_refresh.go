package gateway

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"allin/internal/provider/workbuddy"
)

// creditsRefreshInterval 是积分快照的刷新周期。
// 选号只读内存快照，所以这个周期决定「新拿到的积分多久能参与选号」。
const creditsRefreshInterval = 10 * time.Minute

// creditsRefreshTimeout 是一次全量刷新的总超时。
const creditsRefreshTimeout = 60 * time.Second

// startCreditsRefresher 定期拉取 WorkBuddy 账号积分并写入账号池。
//
// 只刷新 WorkBuddy：积分感知选号依赖「额度会到期、过期作废」这个语义，
// 其它供应商没有等价的到期概念，不参与。
func (s *Server) startCreditsRefresher(ctx context.Context) {
	refresh := func() { s.refreshCredits(ctx) }

	go func() {
		// 启动时先刷一次：否则重启后头 10 分钟选号拿不到积分信息
		refresh()

		t := time.NewTicker(creditsRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
}

// refreshCredits 查一遍所有 WorkBuddy 账号的积分，写进账号池。
func (s *Server) refreshCredits(parent context.Context) {
	cfg := s.store.Snapshot()

	type job struct {
		providerID string
		accountID  string
		customer   workbuddy.Customer
	}
	var jobs []job

	for _, p := range cfg.Providers {
		if p == nil || (p.ID != "workbuddy" && p.ID != "workbuddy-ai") {
			continue
		}
		for _, a := range p.Accounts {
			if a == nil || !a.IsEnabled() || a.UID == "" || a.Key() == "" {
				continue
			}
			jobs = append(jobs, job{
				providerID: p.ID,
				accountID:  a.ID,
				customer:   workbuddy.NewCustomer(a.UID, a.Key(), a.Domain, "", "", p.ID == "workbuddy-ai"),
			})
		}
	}
	if len(jobs) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(parent, creditsRefreshTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sum, err := workbuddy.GetCredits(ctx, j.customer)
			if err != nil {
				// 查不到就保留旧快照，让它自然过期失效，不要清零余额
				log.Printf("积分刷新失败: %s/%s: %v", j.providerID, j.accountID, err)
				// 凭据本身失效（401/403）的账号直接在账号池里标记失败，
				// 让 Pick 跳过它。否则每次请求都可能选中一个死号，
				// 白等一轮上游往返再换号，首字延迟白白翻倍。
				if isAuthError(err) {
					s.pool.Fail(j.providerID, j.accountID, 401, "积分接口鉴权失败")
				}
				return
			}
			if sum == nil {
				return
			}
			var resetAt time.Time
			if t, ok := parseCreditTime(sum.ResetAt); ok {
				resetAt = t
			}
			s.pool.SetCreditsFull(j.providerID, j.accountID, sum.Remaining, sum.Total, resetAt)
		}(j)
	}
	wg.Wait()
}

// parseCreditTime 解析 CreditSummary.ResetAt（格式 "2006-01-02 15:04"，UTC+8）。
func parseCreditTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	loc := time.FixedZone("CST", 8*3600)
	t, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// isAuthError 判断积分查询失败是不是「凭据失效」这一类。
// 上游对失效 token 返回 401/403，错误信息里带 http=401 / http=403。
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "http=401") ||
		strings.Contains(msg, "http=403") ||
		strings.Contains(msg, "401 Authorization Required") ||
		strings.Contains(msg, "403 Forbidden")
}
