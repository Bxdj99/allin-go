package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"allin/internal/config"
)

// 全套任务（POST /api/workbuddy/tasks）这条路径必须把签到结果写回配置。
//
// 踩过的坑：签到闭包从 s.store.Snapshot() 取账号去调 doCheckin，
// 而 Snapshot 是深拷贝 —— 改完就丢。结果 CheckinDate 永远停在旧日期，
// 面板上看起来像「从来没签到过」，用户据此判断自动签到没生效。
// 单账号路径（admin.go）一直是好的，只有全套任务路径漏了回写，
// 而自动签到跑的正是全套任务。

func newTasksTestServer(t *testing.T) (*Server, string, *config.Store) {
	t.Helper()

	// 模拟上游：签到接口返回 10001「今天已签到」，其它一律简单成功。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "daily-checkin") {
			_, _ = w.Write([]byte(`{"code":10001,"msg":"already"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	st, err := config.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	cfg := st.Snapshot()
	cfg.Providers = []*config.Provider{{
		ID:      "workbuddy",
		Name:    "WorkBuddy",
		Enabled: boolPtr(true),
		BaseURL: up.URL,
		Kind:    "workbuddy",
		Accounts: []*config.Account{{
			ID:      "acc-1",
			Name:    "测试号",
			Type:    "token",
			Enabled: boolPtr(true),
			Access:  "tok-abc",
			UID:     "uid-1",
			// 故意放一个很旧的日期，模拟线上那些停在 11-17 的账号
			CheckinDate: "2026-11-17",
		}},
	}}
	if err := st.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	return &Server{store: st}, path, st
}

func TestTasksWriteBackCheckinDate(t *testing.T) {
	s, path, _ := newTasksTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/workbuddy/tasks", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleWorkBuddyTasks(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", w.Code, w.Body.String())
	}

	// 落盘后重新读，确认签到日期真的写进去了
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	var reloaded config.Config
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("解析配置: %v", err)
	}

	p := reloaded.FindProvider("workbuddy")
	if p == nil || len(p.Accounts) == 0 {
		t.Fatal("配置里找不到 workbuddy 账号")
	}
	a := p.Accounts[0]

	if a.CheckinDate != today() {
		t.Errorf("CheckinDate = %q, 期望今天 %q（全套任务路径没有回写签到记录）", a.CheckinDate, today())
	}
	if a.CheckinOutcome != "already" {
		t.Errorf("CheckinOutcome = %q, 期望 already", a.CheckinOutcome)
	}
}

// 回写只应影响本轮账号，不能把别的账号的旧记录清掉。
func TestTasksWriteBackOnlyTouchesRanAccount(t *testing.T) {
	s, path, st := newTasksTestServer(t)

	cfg := st.Snapshot()
	cfg.Providers[0].Accounts = append(cfg.Providers[0].Accounts, &config.Account{
		ID:          "acc-2",
		Name:        "另一个号",
		Type:        "token",
		Enabled:     boolPtr(false), // 禁用 → 本轮跳过
		Access:      "tok-xyz",
		UID:         "uid-2",
		CheckinDate: "2026-11-01",
	})
	if err := st.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/workbuddy/tasks", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleWorkBuddyTasks(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", w.Code, w.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	var reloaded config.Config
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("解析配置: %v", err)
	}
	p := reloaded.FindProvider("workbuddy")

	var ran, skipped *config.Account
	for _, a := range p.Accounts {
		switch a.ID {
		case "acc-1":
			ran = a
		case "acc-2":
			skipped = a
		}
	}
	if ran == nil || skipped == nil {
		t.Fatal("账号缺失")
	}
	if ran.CheckinDate != today() {
		t.Errorf("跑过的账号 CheckinDate = %q, 期望 %q", ran.CheckinDate, today())
	}
	if skipped.CheckinDate != "2026-11-01" {
		t.Errorf("跳过的账号 CheckinDate 被改动 = %q, 期望保持 2026-11-01", skipped.CheckinDate)
	}
}
