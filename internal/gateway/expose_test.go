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
	"allin/internal/provider"
)

// newExposeTestServer 构造一个只有两个供应商的临时网关。
func newExposeTestServer(t *testing.T) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	seed := `{"server":{"host":"127.0.0.1","port":3425,"authKey":"k"},` +
		`"providers":[{"id":"wb","kind":"openai","apiKey":"sk-test","models":["a","b","c"]},` +
		`{"id":"other","kind":"openai","apiKey":"sk-test","models":["x"]}],` +
		`"routing":{"bareModelFallback":true}}`
	if err := writeFile(t, path, seed); err != nil {
		t.Fatal(err)
	}
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return New(store)
}

func writeFile(t *testing.T, path, body string) error {
	t.Helper()
	return os.WriteFile(path, []byte(body), 0o600)
}

// 走真实 mux，模拟面板「保存筛选」后 /v1/models 立即变化。
func TestProviderExposeFlow(t *testing.T) {
	s := newExposeTestServer(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}

	// 初始：老配置没有 exposedModels，全部暴露
	if got := modelIDs(t, s); len(got) != 4 {
		t.Fatalf("初始应 4 个模型，得到 %v", got)
	}

	// 只暴露 b
	w := post("/api/providers/models/expose", `{"providerId":"wb","models":["b"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}
	got := modelIDs(t, s)
	if len(got) != 2 || !contains(got, "wb/b") || !contains(got, "other/x") {
		t.Fatalf("保存后模型列表不对: %v", got)
	}

	// 未暴露的模型名调用应被拒绝
	cfg := s.store.Snapshot()
	if _, _, _, err := provider.Resolve(cfg, "wb/a"); err == nil {
		t.Fatal("未暴露的模型不该能解析成功")
	}
	// 裸模型名兜底也要受 exposedModels 约束："a" 没勾选，不该被找到
	if _, _, _, err := provider.Resolve(cfg, "a"); err == nil {
		t.Fatal("未暴露的模型不该能通过裸模型名解析")
	}
	if _, _, m, err := provider.Resolve(cfg, "b"); err != nil || m != "b" {
		t.Fatalf("已暴露的模型应能通过裸模型名解析: %v %v", m, err)
	}
	if _, _, m, err := provider.Resolve(cfg, "wb/b"); err != nil || m != "b" {
		t.Fatalf("暴露的模型应能解析: %v %v", m, err)
	}

	// 全不选：一个都不暴露，但 supplier 仍在配置里
	w = post("/api/providers/models/expose", `{"providerId":"wb","models":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("全不选失败: %d %s", w.Code, w.Body.String())
	}
	got = modelIDs(t, s)
	if len(got) != 1 || got[0] != "other/x" {
		t.Fatalf("全不选后应只剩 other/x: %v", got)
	}

	// 传了不存在的模型：400，且不改配置
	w = post("/api/providers/models/expose", `{"providerId":"wb","models":["b","nope"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知模型应 400，得到 %d", w.Code)
	}

	// 供应商不存在：404
	w = post("/api/providers/models/expose", `{"providerId":"ghost","models":[]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知供应商应 404，得到 %d", w.Code)
	}

	// GET /api/providers 里要带上 exposedModels / exposed
	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	req.Header.Set("Authorization", "Bearer k")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var resp struct {
		Providers []struct {
			ID            string   `json:"id"`
			Models        []string `json:"models"`
			ExposedModels []string `json:"exposedModels"`
			Exposed       []string `json:"exposed"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range resp.Providers {
		if p.ID != "wb" {
			continue
		}
		found = true
		if len(p.Models) != 3 {
			t.Fatalf("models 本体不该被筛选改动: %v", p.Models)
		}
		if len(p.Exposed) != 0 {
			t.Fatalf("全不选后 exposed 应为空: %v", p.Exposed)
		}
		if len(p.ExposedModels) == 0 {
			t.Fatal("exposedModels 应落盘为哨兵值而不是空")
		}
	}
	if !found {
		t.Fatal("返回里找不到 wb")
	}
}

// 勾选结果不能被「同步模型目录」冲掉。
func TestExposeSurvivesSync(t *testing.T) {
	p := &config.Provider{ID: "wb", Models: []string{"a"}, ExposedModels: []string{"a"}}
	merged, added, _ := mergeModels(p.Models, []string{"a", "b"})
	if len(added) != 1 {
		t.Fatalf("应新增 b: %v", added)
	}
	p.Models = merged
	if got := p.Exposed(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("新同步进来的模型不该自动暴露: %v", got)
	}
}

func modelIDs(t *testing.T, s *Server) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(resp.Data))
	for _, d := range resp.Data {
		out = append(out, d.ID)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
