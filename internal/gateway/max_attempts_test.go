package gateway

import (
	"path/filepath"
	"testing"

	"allin/internal/config"
)

// 「最大尝试账号数」必须是真配置。
//
// 之前是写死的 const，面板上能改但不生效 —— 用户设成 1 仍按 3 跑，
// 实测发现后才修。这里锁住「改了要真生效」。

func newServerWithMaxAttempts(t *testing.T, v int) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := config.NewStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := st.Snapshot()
	cfg.Server.SwitchPolicy.MaxAttempts = &v
	if err := st.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return &Server{store: st}
}

func TestMaxAccountAttemptsReadsConfig(t *testing.T) {
	for _, v := range []int{1, 2, 5, 10} {
		s := newServerWithMaxAttempts(t, v)
		if got := s.maxAccountAttempts(); got != v {
			t.Errorf("配置 %d 时应返回 %d，实际 %d", v, v, got)
		}
	}
}

// 未配置时用缺省 3。
func TestMaxAccountAttemptsDefault(t *testing.T) {
	dir := t.TempDir()
	st, err := config.NewStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s := &Server{store: st}
	if got := s.maxAccountAttempts(); got != defaultMaxAccountAttempts {
		t.Errorf("缺省应为 %d，实际 %d", defaultMaxAccountAttempts, got)
	}
}

// store 为 nil 时不能 panic（单测/异常路径）。
func TestMaxAccountAttemptsNilStore(t *testing.T) {
	s := &Server{}
	if got := s.maxAccountAttempts(); got != defaultMaxAccountAttempts {
		t.Errorf("store 为 nil 时应回缺省，实际 %d", got)
	}
}

// 越界值由 config 层夹住：0 回落 3，超 10 夹到 10。
func TestMaxAccountAttemptsBounds(t *testing.T) {
	s := newServerWithMaxAttempts(t, 0)
	if got := s.maxAccountAttempts(); got != 3 {
		t.Errorf("0 应回落 3，实际 %d", got)
	}
	s = newServerWithMaxAttempts(t, 99)
	if got := s.maxAccountAttempts(); got != 10 {
		t.Errorf("99 应夹到 10，实际 %d", got)
	}
}
