package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 账号用量统计的持久化。
//
// 之前的统计只存在内存里，网关一重启就全清零 —— 用户根本没法用它回答
// 「这个月积分怎么花的」。这里把累计值落盘，重启后接着累加。
//
// 设计取舍：
//   - 只存累计值（不做按天分桶）：文件小、写入少，够回答「谁花了多少」
//   - 每 2 分钟或累计变更达到阈值时落盘，避免每次请求都写文件
//   - 落盘失败不影响统计本身（内存里照常累加），只记日志

const (
	accUsageSaveInterval = 2 * time.Minute
	accUsageFileName     = "account-usage.json"
)

// accUsageSnapshot 是落盘格式。
type accUsageSnapshot struct {
	SavedAt int64              `json:"savedAt"`
	Rows    []accountUsageRow  `json:"rows"`
}

var (
	accUsageSaveMu   sync.Mutex
	accUsageDirty    bool
	accUsageSaveOnce sync.Once
)

// markAccUsageDirty 标记有变更待落盘。
func markAccUsageDirty() {
	accUsageSaveMu.Lock()
	accUsageDirty = true
	accUsageSaveMu.Unlock()
}

// accUsagePath 返回落盘路径（与配置文件同目录）。
func (s *Server) accUsagePath() string {
	dir := filepath.Dir(s.store.Path())
	if dir == "" || dir == "." {
		return accUsageFileName
	}
	return filepath.Join(dir, accUsageFileName)
}

// loadAccountUsage 从磁盘恢复统计。启动时调用一次。
func (s *Server) loadAccountUsage() {
	raw, err := os.ReadFile(s.accUsagePath())
	if err != nil {
		return // 首次运行没有文件，正常
	}
	var snap accUsageSnapshot
	if json.Unmarshal(raw, &snap) != nil {
		return
	}
	accUsageMu.Lock()
	defer accUsageMu.Unlock()
	for _, r := range snap.Rows {
		k := accUsageKey(r.ProviderID, r.AccountID)
		st := &accountUsage{}
		st.Requests.Store(r.Requests)
		st.Errors.Store(r.Errors)
		st.InTokens.Store(r.InTokens)
		st.OutTokens.Store(r.OutTokens)
		st.CreditMilli.Store(int64(r.Credit*1000 + 0.5))
		st.LastAt.Store(r.LastAt)
		accUsageTable[k] = st
	}
}

// saveAccountUsage 把当前统计写入磁盘。
func (s *Server) saveAccountUsage() {
	rows := accountUsageRows()

	accUsageSaveMu.Lock()
	if !accUsageDirty {
		accUsageSaveMu.Unlock()
		return
	}
	accUsageDirty = false
	accUsageSaveMu.Unlock()

	snap := accUsageSnapshot{SavedAt: time.Now().Unix(), Rows: rows}
	buf, err := json.Marshal(snap)
	if err != nil {
		return
	}
	path := s.accUsagePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// startAccountUsageSaver 启动后台落盘循环。
func (s *Server) startAccountUsageSaver(ctx context.Context) {
	go func() {
		t := time.NewTicker(accUsageSaveInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.saveAccountUsage() // 退出前再存一次
				return
			case <-t.C:
				s.saveAccountUsage()
			}
		}
	}()
}
