package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// 质量统计与工具统计的持久化。
//
// 之前这两份数据只存内存，网关一重启就归零 —— 而它们恰恰是
// 「回答这个月积分怎么花的」需要的长期数据，清掉就没意义了。
//
// 设计取舍（与 account_usage_persist 一致）：
//   - 只存累计值，不做按天分桶：文件小、写入少
//   - 定期落盘 + 退出前落盘，避免每次请求都写文件
//   - 落盘失败不影响统计本身，只记日志
//   - 原子替换（写 .tmp 再 rename），防止半截文件

const (
	qualityFileName     = "quality-stats.json"
	qualitySaveInterval = 30 * time.Second
	// qualityFlushDelay 是「变更后多久落盘」。
	// 5 秒窗口足够把一轮请求的变更合并成一次写，
	// 又不至于在 kill -9 时丢掉太多。
	qualityFlushDelay = 5 * time.Second
)

var (
	qualitySaveMu       sync.Mutex
	qualityDirty        bool
	qualityFlushPending bool
	qualityPath         string
	qualitySaveOnce     sync.Once
)

// qualitySnapshot 是落盘格式。
type qualitySnapshot struct {
	SavedAt int64               `json:"savedAt"`
	Errors  map[string]int64    `json:"errors,omitempty"`
	Trim    qualityTrimSnapshot `json:"trim"`
	Reqs    qualityReqSnapshot  `json:"requests"`
	Tools   []ToolUsageRow      `json:"tools,omitempty"`
	ToolReq int64               `json:"toolRequests"`
}

type qualityTrimSnapshot struct {
	Hits   int64 `json:"hits"`
	Saved  int64 `json:"saved"`
	Before int64 `json:"before"`
}

type qualityReqSnapshot struct {
	Requests int64 `json:"requests"`
	Attempts int64 `json:"attempts"`
}

// markQualityDirty 标记有变更待落盘，并在 5 秒后异步写一次。
//
// 为什么要 debounce 而不是等定时器：进程被 kill -9 时收不到信号，
// 定期落盘会丢掉最后一次写盘之后的全部数据。
// 5 秒窗口足够把一轮请求的变更合并成一次写，又不至于丢太多。
func markQualityDirty() {
	qualitySaveMu.Lock()
	qualityDirty = true
	already := qualityFlushPending
	qualityFlushPending = true
	qualitySaveMu.Unlock()

	if already {
		return
	}
	go func() {
		time.Sleep(qualityFlushDelay)
		qualitySaveMu.Lock()
		qualityFlushPending = false
		qualitySaveMu.Unlock()
		saveQuality()
	}()
}

// qualityFilePath 计算落盘路径（与 config.json 同目录）。
func qualityFilePath() string {
	if qualityPath != "" {
		return qualityPath
	}
	return qualityFileName
}

// initQualityPersist 设置落盘路径并尝试恢复。
func (s *Server) initQualityPersist() {
	dir := filepath.Dir(s.store.Path())
	if dir != "" && dir != "." {
		qualityPath = filepath.Join(dir, qualityFileName)
	} else {
		qualityPath = qualityFileName
	}
	loadQuality()
}

// loadQuality 启动时读回累计值。
func loadQuality() {
	raw, err := os.ReadFile(qualityFilePath())
	if err != nil {
		return // 首次运行没有文件，正常
	}
	var snap qualitySnapshot
	if json.Unmarshal(raw, &snap) != nil {
		return
	}

	quality.errMu.Lock()
	quality.errCnts = map[string]*atomic.Int64{}
	for k, v := range snap.Errors {
		if v <= 0 {
			continue
		}
		c := &atomic.Int64{}
		c.Store(v)
		quality.errCnts[k] = c
	}
	quality.errMu.Unlock()

	quality.TrimHits.Store(snap.Trim.Hits)
	quality.TrimSaved.Store(snap.Trim.Saved)
	quality.TrimBefore.Store(snap.Trim.Before)
	quality.Requests.Store(snap.Reqs.Requests)
	quality.Attempts.Store(snap.Reqs.Attempts)

	toolUsageMu.Lock()
	toolUsageTable = map[string]*toolUsageStat{}
	for _, r := range snap.Tools {
		if r.Name == "" {
			continue
		}
		st := &toolUsageStat{}
		st.declared.Store(r.Declared)
		st.used.Store(r.Used)
		toolUsageTable[r.Name] = st
	}
	toolUsageMu.Unlock()
	toolUsageRequests.Store(snap.ToolReq)
}

// saveQuality 把当前累计值写盘。
func saveQuality() {
	qualitySaveMu.Lock()
	if !qualityDirty {
		qualitySaveMu.Unlock()
		return
	}
	qualityDirty = false
	qualitySaveMu.Unlock()

	snap := qualitySnapshot{
		SavedAt: time.Now().Unix(),
		Trim: qualityTrimSnapshot{
			Hits:   quality.TrimHits.Load(),
			Saved:  quality.TrimSaved.Load(),
			Before: quality.TrimBefore.Load(),
		},
		Reqs: qualityReqSnapshot{
			Requests: quality.Requests.Load(),
			Attempts: quality.Attempts.Load(),
		},
		ToolReq: toolUsageRequests.Load(),
	}
	snap.Errors = map[string]int64{}
	quality.errMu.RLock()
	for k, c := range quality.errCnts {
		if v := c.Load(); v > 0 {
			snap.Errors[k] = v
		}
	}
	quality.errMu.RUnlock()

	snap.Tools = toolUsageRows()

	raw, err := json.Marshal(snap)
	if err != nil {
		return
	}
	path := qualityFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// startQualitySaver 定期落盘，并在 ctx 结束时再存一次。
func (s *Server) startQualitySaver(done <-chan struct{}) {
	qualitySaveOnce.Do(func() {
		go func() {
			t := time.NewTicker(qualitySaveInterval)
			defer t.Stop()
			for {
				select {
				case <-done:
					saveQuality()
					return
				case <-t.C:
					saveQuality()
				}
			}
		}()
	})
}

// FlushQuality 立即把统计写盘。
//
// 为什么要单独暴露：进程被 kill 时 ctx 不一定来得及走完，
// 定期落盘会丢掉最后一段。实测过 —— 重启后 requests 从 11 掉回 7，
// 因为磁盘上还是 30 秒前的快照。
func FlushQuality() {
	qualitySaveMu.Lock()
	qualityDirty = true // 强制写一次
	qualitySaveMu.Unlock()
	saveQuality()
}
