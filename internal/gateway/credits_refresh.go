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
//
// v1.39.0 从 10 分钟收紧到 90 秒，必须小于 provider.creditsStale（2 分钟）的一半，
// 否则快照会长期处于「过期」状态，保底与选号一起退化成未知。
// 对齐上游 AccountRoutingPolicy 的 120 秒新鲜期。
//
// 代价：每轮会打 N 次积分接口（N=WorkBuddy 账号数）。为免刷屏与风控，
// refreshCredits 对「凭据失效」的账号做了退避，见该函数。
const creditsRefreshInterval = 90 * time.Second

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
			// 凭据已判死的账号跳过本轮：它不会因为再刷一次积分就恢复，
			// 每 90 秒重打只会刷屏（线上曾累积 882 条 401 失败日志）。
			// 用户重新登录后 pool.Success/MarkNeedRelogin(false) 会清掉标记。
			if s.pool.NeedsRelogin(p.ID, a.ID) {
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
				if isAuthError(err) {
					// 鉴权失败：下面会打 needRelogin 标记，之后的轮次直接跳过，
					// 所以这里只值得记一次，不必每 90 秒刷一条。
					log.Printf("积分刷新失败（凭据失效，后续轮次跳过）: %s/%s", j.providerID, j.accountID)
				} else {
					log.Printf("积分刷新失败: %s/%s: %v", j.providerID, j.accountID, err)
				}
				// 凭据本身失效（401/403）的账号直接在账号池里标记失败，
				// 让 Pick 跳过它。否则每次请求都可能选中一个死号，
				// 白等一轮上游往返再换号，首字延迟白白翻倍。
				if isAuthError(err) {
					s.pool.Fail(j.providerID, j.accountID, 401, "积分接口鉴权失败")
					// 积分接口返回 401 是确定性信号（这里只查余额，不涉及推理），
					// 直接打 needRelogin 标记，让后续轮次跳过它。
					// 不这么做的话，本轮 90 秒要等到 pool.Fail 累计 3 次才生效，
					// 期间会白刷 6 分钟的必失败请求。
					s.pool.MarkNeedRelogin(j.providerID, j.accountID, true)
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
			// 用「余额下降量」反推这段时间的积分消耗。
			//
			// 上游的聊天响应里**不返回 credit 字段**（实测确认），
			// 所以没法按次记账。但余额是准的，两次刷新之间的下降量
			// 就是这段时间真实花掉的积分，按账号累计即可。
			//
			// 注意只在「余额下降」时记：余额上升是重置或充值，不算消耗。
			if cfg.Server.CreditStatsEnabled() {
				if prev, ok := s.pool.LastRemaining(j.providerID, j.accountID); ok && prev > sum.Remaining {
					s.recordCreditDelta(j.providerID, j.accountID, prev-sum.Remaining)
				}
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
