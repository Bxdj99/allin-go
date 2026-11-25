package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 哨兵文件是 service.sh / guard.sh 读自动任务开关的唯一可靠来源。
// 早期 shell 侧用 busybox sed 的 BRE 分支 `\(true\|false\)` 去抠多行 JSON，
// 而 busybox sed 不支持 `\|`，导致开关永远读不到、永远回退 true，
// 面板上「关闭自动任务」从来不起作用。这里锁住格式与取值。

func readScheduleState(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ScheduleStateName))
	if err != nil {
		t.Fatalf("读哨兵失败: %v", err)
	}
	return string(raw)
}

func TestScheduleStateWrittenOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	st, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	_ = st

	got := readScheduleState(t, dir)
	// 首次运行没有显式配置：默认开启，小时默认 9 与 21
	want := "autoCheckin=1\ncheckinHours=9,21\n"
	if got != want {
		t.Errorf("首次运行哨兵 = %q, want %q", got, want)
	}
}

func TestScheduleStateReflectsExplicitOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	st, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	cfg := st.Snapshot()
	off := false
	cfg.Schedule.AutoCheckin = &off
	cfg.Schedule.CheckinHours = []int{10, 22}
	if err := st.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := readScheduleState(t, dir)
	want := "autoCheckin=0\ncheckinHours=10,22\n"
	if got != want {
		t.Errorf("关闭后哨兵 = %q, want %q", got, want)
	}
}

// 已有 config.json（升级上来的设备）加载后也必须补写哨兵，
// 否则 shell 读不到会回退成默认的「开启」，把用户关掉的自动任务又打开。
func TestScheduleStateWrittenOnExistingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	src := t.TempDir()
	srcPath := filepath.Join(src, "config.json")
	a, err := NewStore(srcPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := a.Snapshot()
	off := false
	cfg.Schedule.AutoCheckin = &off
	cfg.Schedule.CheckinHours = []int{7}
	if err := a.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("读源配置: %v", err)
	}
	// 只搬 config.json，故意不搬哨兵，模拟老版本升级
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}

	if _, err := NewStore(path); err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	got := readScheduleState(t, dir)
	want := "autoCheckin=0\ncheckinHours=7\n"
	if got != want {
		t.Errorf("升级后哨兵 = %q, want %q", got, want)
	}
}

// 小时越界要丢弃；全越界时退回默认，不能写出空 checkinHours= 让 shell 判成无小时。
func TestScheduleStateNormalizesHours(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	st, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	cfg := st.Snapshot()
	cfg.Schedule.CheckinHours = []int{25, -1, 3}
	if err := st.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := readScheduleState(t, dir)
	want := "autoCheckin=1\ncheckinHours=3\n"
	if got != want {
		t.Errorf("越界小时哨兵 = %q, want %q", got, want)
	}

	cfg2 := st.Snapshot()
	cfg2.Schedule.CheckinHours = []int{99}
	if err := st.Save(cfg2); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got = readScheduleState(t, dir)
	want = "autoCheckin=1\ncheckinHours=9,21\n"
	if got != want {
		t.Errorf("全越界哨兵 = %q, want %q", got, want)
	}
}

// 哨兵内容必须能被 shell 的朴素切分正确读出：
// 单行、无空格、固定键名。
func TestScheduleStateFormatIsShellFriendly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	st, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := st.Snapshot()
	yes := true
	cfg.Schedule.AutoCheckin = &yes
	cfg.Schedule.CheckinHours = []int{9, 21}
	if err := st.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw := readScheduleState(t, dir)
	for _, ch := range raw {
		switch ch {
		case '\n', '\r', '=':
			continue
		case ' ', '\t':
			t.Fatalf("哨兵不应包含空白字符，实际: %q", raw)
		}
	}
	// 每行形如 key=value
	if got := raw; got != "autoCheckin=1\ncheckinHours=9,21\n" {
		t.Errorf("格式不符: %q", got)
	}
}
