package gateway

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
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

// 刷新失败后的退避。断网时不该每 90 秒都去撞一次。
//
// 实测：11-27 凌晨断网 8 小时，产生 1189 条「积分刷新失败」，
// 全是同一句「network is unreachable」。既刷屏又白耗电量。
// 现在连续失败就逐步拉长间隔，一成功立刻回到 90 秒。
var (
	// creditsMu 保护下面两个字段：刷新在 goroutine 里，读间隔在主循环。
	creditsMu         sync.Mutex
	creditsFailStreak int           // 连续失败轮数
	creditsBackoff    time.Duration // 当前退避时长（预留）
)

// 退避阶梯：90s（正常）→ 3min → 6min → 12min → 24min（封顶）
const creditsBackoffMax = 24 * time.Minute

// nextRefreshDelay 根据连续失败次数算下一轮该等多久。
func nextRefreshDelay() time.Duration {
	creditsMu.Lock()
	streak := creditsFailStreak
	creditsMu.Unlock()
	if streak <= 0 {
		return creditsRefreshInterval
	}
	d := creditsRefreshInterval
	for i := 0; i < streak; i++ {
		d *= 2
		if d >= creditsBackoffMax {
			return creditsBackoffMax
		}
	}
	return d
}

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

		// 用动态间隔而不是固定 Ticker：连续失败要退避，
		// 否则断网时会每 90 秒撞一次，白耗电还刷屏。
		for {
			d := nextRefreshDelay()
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
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
	// 记录本轮成败，用于退避判断。全失败才退避，
	// 部分成功（比如 4 个 401 死号 + 8 个正常）不该拉长间隔。
	var okCount, failCount atomic.Int64
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sum, err := workbuddy.GetCredits(ctx, j.customer)
			if err != nil {
				failCount.Add(1)
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
					// 走 pool.Fail 的累计逻辑，而不是首次就判死。
					//
					// 之前这里首次 401 就打 needRelogin，等于一条 401 就废掉账号。
					// 而 401 也可能来自瞬时故障（网络中间层、上游抖动），
					// 一刀切会把还能用的号误杀 —— 实测有账号因此被永久跳过。
					// pool.Fail 自己会在连续 3 次后标 needRelogin，够了。
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
			okCount.Add(1)
		}(j)
	}
	wg.Wait()

	// 退避判断：只有「一个都没成功」才认为网络/上游整体不可用。
	// 部分失败是账号自身问题（401 死号），不该拖慢整体刷新。
	if okCount.Load() == 0 && failCount.Load() > 0 {
		creditsMu.Lock()
		creditsFailStreak++
		first := creditsFailStreak == 1
		creditsMu.Unlock()
		if first {
			log.Printf("积分刷新全部失败，进入退避（下一轮 %v 后）", nextRefreshDelay())
		}
	} else {
		creditsMu.Lock()
		was := creditsFailStreak
		creditsFailStreak = 0
		creditsMu.Unlock()
		if was > 0 {
			log.Printf("积分刷新恢复（此前连续失败 %d 轮）", was)
		}
	}
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
