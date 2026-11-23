package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"allin/internal/config"
)

// 上游有两种「10001 今天已签到」：
//   1. 真的签过了     → heatmap 当天 score > 0
//   2. 状态位被占但没签 → heatmap 当天 score == 0
// 后者以前被判成 already 写进 checkinDate，面板显示「今日已签」但积分没到账，
// 而且当天不再重试。这里锁住区分逻辑。
func TestDoCheckinStaleFlag(t *testing.T) {
	cases := []struct {
		name       string
		score      int
		withCell   bool
		wantOut    string
		wantErr    bool
		wantCredit int
	}{
		{name: "真的签过了", score: 12, withCell: true, wantOut: "already"},
		{name: "状态位被占但当天没签", score: 0, withCell: true, wantOut: "stale_flag", wantErr: true},
		{name: "heatmap 没有今天这一格", withCell: false, wantOut: "already"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/v2/billing/meter/daily-checkin", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到，请明天再来"}`))
			})
			mux.HandleFunc("/activity/growth/heatmap", func(w http.ResponseWriter, r *http.Request) {
				cells := []map[string]any{}
				if tc.withCell {
					cells = append(cells, map[string]any{
						"date":  today(),
						"score": tc.score,
					})
				}
				body, _ := json.Marshal(map[string]any{
					"code": 0,
					"data": map[string]any{"cells": cells},
				})
				_, _ = w.Write(body)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			s := &Server{}
			a := &config.Account{ID: "wb-test", UID: "u1", Access: "tok", Domain: "copilot.tencent.com"}
			credit, _, outcome, err := s.doCheckin(srv.URL, a)

			if outcome != tc.wantOut {
				t.Fatalf("outcome = %q, want %q", outcome, tc.wantOut)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if credit != tc.wantCredit {
				t.Fatalf("credit = %d, want %d", credit, tc.wantCredit)
			}
		})
	}
}

// 上游正常返回 code:0 时不应去查 heatmap，也不该被降级。
func TestDoCheckinOKSkipsHeatmap(t *testing.T) {
	heatmapHits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/billing/meter/daily-checkin", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"credit":100,"streak_days":45}}`))
	})
	mux.HandleFunc("/activity/growth/heatmap", func(w http.ResponseWriter, r *http.Request) {
		heatmapHits++
		_, _ = w.Write([]byte(`{"code":0,"data":{"cells":[]}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &Server{}
	a := &config.Account{ID: "wb-test", UID: "u1", Access: "tok"}
	credit, streak, outcome, err := s.doCheckin(srv.URL, a)
	if err != nil || outcome != "ok" || credit != 100 || streak != 45 {
		t.Fatalf("got credit=%d streak=%d outcome=%q err=%v", credit, streak, outcome, err)
	}
	if heatmapHits != 0 {
		t.Fatalf("code:0 时不该查 heatmap，实际查了 %d 次", heatmapHits)
	}
}

// heatmap 查询失败时保持上游判断，不能因为查不到就推翻「已签到」。
func TestDoCheckinHeatmapUnreachable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/billing/meter/daily-checkin", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到，请明天再来"}`))
	})
	mux.HandleFunc("/activity/growth/heatmap", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &Server{}
	a := &config.Account{ID: "wb-test", UID: "u1", Access: "tok"}
	_, _, outcome, err := s.doCheckin(srv.URL, a)
	if err != nil || outcome != "already" {
		t.Fatalf("heatmap 不可达时应保持 already，实际 outcome=%q err=%v", outcome, err)
	}
}

// 10001 的 data 为空，streak 必须补查，否则「连登 0 天」写进配置。
func TestDoCheckinAlreadyBackfillsStreak(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/billing/meter/daily-checkin", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到，请明天再来"}`))
	})
	mux.HandleFunc("/activity/growth/heatmap", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"cells":[{"date":"` + today() + `","score":12}]}}`))
	})
	mux.HandleFunc("/activity/growth/streak", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":45}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &Server{}
	a := &config.Account{ID: "wb-test", UID: "u1", Access: "tok"}
	_, streak, outcome, err := s.doCheckin(srv.URL, a)
	if err != nil || outcome != "already" {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if streak != 45 {
		t.Fatalf("streak = %d, want 45（应从 growth/streak 补齐）", streak)
	}
}
