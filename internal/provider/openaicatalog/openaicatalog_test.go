package openaicatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestJoinModelsURL baseURL 的几种常见写法都要拼对。
func TestJoinModelsURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1/models"},
		{"https://api.deepseek.com/v1/", "https://api.deepseek.com/v1/models"},
		{"https://api.openai.com", "https://api.openai.com/v1/models"},
		{"https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/models"},
		{"https://api.siliconflow.cn/v1/models", "https://api.siliconflow.cn/v1/models"},
	}
	for _, c := range cases {
		got, err := JoinModelsURL(c.in)
		if err != nil {
			t.Errorf("JoinModelsURL(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("JoinModelsURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "not-a-url", "ftp://x/v1"} {
		if _, err := JoinModelsURL(bad); err == nil {
			t.Errorf("JoinModelsURL(%q) 应报错", bad)
		}
	}
}

// TestFetchStandard 标准 OpenAI 形态：{"data":[{"id":...}]}。
func TestFetchStandard(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o"},{"id":"o3-mini"},{"id":"gpt-4o"}]}`))
	}))
	defer srv.Close()

	ids, err := Fetch(context.Background(), srv.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path = %q", gotPath)
	}
	if strings.Join(ids, ",") != "gpt-4o,o3-mini" {
		t.Fatalf("ids = %v", ids)
	}
}

// TestFetchModelsKey 少数实现把列表放在 models 键下。
func TestFetchModelsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer srv.Close()

	ids, err := Fetch(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("ids = %v", ids)
	}
}

// TestFetchUnauthorized 401 要给出明确错误，不能被当成空列表。
func TestFetchUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	if _, err := Fetch(context.Background(), srv.URL+"/v1", "x"); err == nil {
		t.Fatal("401 应报错")
	}
}

// TestFetchEmpty 空列表视为失败（否则会把用户的模型表当成同步成功）。
func TestFetchEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	if _, err := Fetch(context.Background(), srv.URL+"/v1", "k"); err == nil {
		t.Fatal("空列表应报错")
	}
}
