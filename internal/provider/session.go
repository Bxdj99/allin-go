package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"allin/internal/config"
)

// 会话绑定（sticky routing）。
//
// 为什么需要：WorkBuddy 这类上游对「同一对话频繁换账号」很敏感，
// 表现为 429 限流甚至中断。而一个 agent 会话会连续发几十个请求，
// 每个请求都换号既浪费额度又容易触发风控。
//
// 做法：按「会话指纹」把请求钉在同一个账号上，直到
//   - 该账号余额低于 CreditsFloor（继续用就会请求失败）
//   - 该账号进入冷却（限流/失效）
//   - 会话超时
//
// 会话指纹由客户端传来的对话内容算出来，不依赖客户端配合。

const (
	// sessionTTL 是绑定的存活时间。超过就重新选号。
	sessionTTL = 30 * time.Minute
	// sessionGCThreshold 是触发清理的条目数阈值。
	sessionGCThreshold = 512
)

// sessionBinding 是一条会话绑定。
type sessionBinding struct {
	accountID string
	lastSeen  time.Time
}

// sessionTable 管理会话 → 账号的绑定关系。
type sessionTable struct {
	mu    sync.Mutex
	items map[string]*sessionBinding // key: providerID + "\x00" + sessionKey
}

func newSessionTable() *sessionTable {
	return &sessionTable{items: make(map[string]*sessionBinding)}
}

// SessionKey 从请求体算出会话指纹。
//
// 用途相同的两个请求（同一段对话继续追问）会得到同一个 key。
// 算法取「第一条 user 消息 + 消息数量区间」，因为：
//   - 第一条 user 消息是这段话的开头，整个会话里不变
//   - 只算内容哈希会因每轮追加而变，必须只用开头
func SessionKey(providerID string, fingerprint string) string {
	if fingerprint == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(providerID + "\x00" + fingerprint))
	return hex.EncodeToString(sum[:8])
}

// Bind 记录会话与账号的绑定。
func (t *sessionTable) Bind(key, accountID string) {
	if t == nil || key == "" || accountID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.items[key] = &sessionBinding{accountID: accountID, lastSeen: time.Now()}
	if len(t.items) > sessionGCThreshold {
		t.gcLocked()
	}
}

// Lookup 取出会话当前绑定的账号 id。过期或不存在返回空。
func (t *sessionTable) Lookup(key string) string {
	if t == nil || key == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.items[key]
	if b == nil {
		return ""
	}
	if time.Since(b.lastSeen) > sessionTTL {
		delete(t.items, key)
		return ""
	}
	b.lastSeen = time.Now()
	return b.accountID
}

// Unbind 解除绑定（账号不可用时调用）。
func (t *sessionTable) Unbind(key string) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.items, key)
}

// gcLocked 清理过期条目。调用方须已持锁。
func (t *sessionTable) gcLocked() {
	now := time.Now()
	for k, b := range t.items {
		if now.Sub(b.lastSeen) > sessionTTL {
			delete(t.items, k)
		}
	}
}

// 策略实现 --------------------------------------------------------------

// pickByRotate 轮询：按成功次数最少的选，等价于均摊。
func pickByRotate(cands []*config.Account, st map[string]*acctState, pid string) *config.Account {
	var best *config.Account
	var bestSucc int64 = -1
	for _, a := range cands {
		s := st[key(pid, a.ID)]
		var succ int64
		if s != nil {
			succ = s.successes
		}
		if best == nil || succ < bestSucc {
			best, bestSucc = a, succ
		}
	}
	return best
}

// pickByBalance 用量均衡：优先用完「剩余额度比例」最低的账号，
// 让各账号的余额百分比趋于一致，避免有的号先耗光。
func pickByBalance(cands []*config.Account, st map[string]*acctState, pid string, now time.Time) *config.Account {
	var best *config.Account
	bestPct := 2.0 // 百分比 0~1，初值取大于 1
	for _, a := range cands {
		s := st[key(pid, a.ID)]
		if s == nil || s.creditsAt.IsZero() || now.Sub(s.creditsAt) > creditsStale {
			continue
		}
		total := s.creditTotal
		if total <= 0 {
			continue
		}
		pct := s.remaining / total
		if best == nil || pct < bestPct {
			best, bestPct = a, pct
		}
	}
	if best == nil {
		// 没有积分信息就退回轮询
		return pickByRotate(cands, st, pid)
	}
	return best
}

// pickByExpiry 到期优先：优先用完最早过期的额度。
func pickByExpiry(cands []*config.Account, st map[string]*acctState, pid string, now time.Time) *config.Account {
	var best *config.Account
	var bestReset time.Time
	var unknown []*config.Account
	for _, a := range cands {
		s := st[key(pid, a.ID)]
		if s == nil || s.creditsAt.IsZero() || now.Sub(s.creditsAt) > creditsStale {
			unknown = append(unknown, a)
			continue
		}
		if s.resetAt.IsZero() {
			unknown = append(unknown, a)
			continue
		}
		if best == nil || s.resetAt.Before(bestReset) {
			best, bestReset = a, s.resetAt
		}
	}
	if best != nil {
		return best
	}
	if len(unknown) > 0 {
		return pickByRotate(unknown, st, pid)
	}
	return pickByRotate(cands, st, pid)
}

// 首轮选号也需要分散负载。
//
// sticky 模式下 rank 只比「余额充足 + 到期时间 + 成功次数」，
// 全新会话进来时这些全相同，会全部落到第一个账号上，
// 结果是所有会话挤在一个号里 —— 仍然会触发限流。
//
// 这里按「当前绑定了多少个会话」做平局打破，让新会话铺开。
func (t *sessionTable) loadOf(accountID string) int {
	if t == nil || accountID == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	now := time.Now()
	for _, b := range t.items {
		if b.accountID == accountID && now.Sub(b.lastSeen) <= sessionTTL {
			n++
		}
	}
	return n
}

// Reset 清空全部绑定。
func (t *sessionTable) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.items = map[string]*sessionBinding{}
	t.mu.Unlock()
}
