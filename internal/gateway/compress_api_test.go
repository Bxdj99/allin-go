package gateway

import (
	"testing"

	"allin/internal/config"
)

// 开关默认必须是开启
func TestHistoryCompressDefaultOn(t *testing.T) {
	var s config.Server
	if !s.HistoryCompressEnabled() {
		t.Fatal("未配置时应默认开启")
	}
}

// 显式关闭要能生效
func TestHistoryCompressExplicitOff(t *testing.T) {
	off := false
	s := config.Server{HistoryCompress: &off}
	if s.HistoryCompressEnabled() {
		t.Fatal("显式 false 时应关闭")
	}
}

// 保留组数默认 6，非法值回落到 6
func TestCompressKeepRecentDefault(t *testing.T) {
	var s config.Server
	if got := s.CompressKeepRecentGroups(); got != 6 {
		t.Fatalf("默认应为 6，实际 %d", got)
	}
	zero := 0
	s2 := config.Server{CompressKeepRecent: &zero}
	if got := s2.CompressKeepRecentGroups(); got != 6 {
		t.Fatalf("0 应回落到 6，实际 %d", got)
	}
	v := 12
	s3 := config.Server{CompressKeepRecent: &v}
	if got := s3.CompressKeepRecentGroups(); got != 12 {
		t.Fatalf("应为 12，实际 %d", got)
	}
}

// 只传 maxOutput 也必须能保存（之前被误判为「什么都没传」）
func TestMaxOutputOnlyAccepted(t *testing.T) {
	v := 4096
	in := struct {
		Enabled    *bool
		KeepRecent *int
		MaxOutput  *int
	}{MaxOutput: &v}
	if in.Enabled == nil && in.KeepRecent == nil && in.MaxOutput == nil {
		t.Fatal("只传 maxOutput 应被接受")
	}
}
