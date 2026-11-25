package gateway

import (
	"io"
	"log"
	"os"
	"sync"
)

// 全局日志开关。
//
// 目标：一个开关同时管住
//   - 写进 gateway.log 的 stdout 日志（全部 46 处 log.Printf 都会受影响）
//   - 「日志」页里的请求采样
//
// 做法是给标准库 log 换一个可切换的 Writer，而不是去改每一处调用点。
// 这样新增的日志也自动受控，不会漏掉。

type toggleWriter struct {
	mu      sync.RWMutex
	enabled bool
	out     io.Writer // 关闭时保留，重开时还原
}

func (w *toggleWriter) Write(p []byte) (int, error) {
	w.mu.RLock()
	on := w.enabled
	out := w.out
	w.mu.RUnlock()
	if !on || out == nil {
		// 丢弃但报告成功，避免日志调用方因错误改变行为
		return len(p), nil
	}
	return out.Write(p)
}

func (w *toggleWriter) set(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enabled = on
}

func (w *toggleWriter) isOn() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.enabled
}

var globalLog = &toggleWriter{enabled: true, out: os.Stderr}

// SetupLogging 把标准库 log 的输出接到可控 Writer 上。
// 必须在 main 里、任何 log 输出前调用一次。
func SetupLogging() {
	globalLog.mu.Lock()
	globalLog.out = log.Writer()
	globalLog.mu.Unlock()
	log.SetOutput(globalLog)
}

// SetLogEnabled 打开/关闭全部日志输出。
func SetLogEnabled(on bool) { globalLog.set(on) }

// LogEnabled 当前是否开启。
func LogEnabled() bool { return globalLog.isOn() }

// statsEnabled 判断是否采集统计数据。
//
// 语义：与「日志总开关」完全一致 —— 关掉开关就彻底停采集，
// 不区分「日志输出」和「统计累计」。
// 用户明确要求二者统一，所以这里直接复用同一个配置项。
//
// 注意：关掉后已有数据仍在，只是不再增长；重新打开会接着累加。
func (s *Server) statsEnabled() bool {
	if s == nil || s.store == nil {
		return true
	}
	return s.store.Snapshot().Server.LogCaptureEnabled()
}
