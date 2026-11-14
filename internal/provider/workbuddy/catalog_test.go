package workbuddy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubRoundTripper 把请求重定向到本地处理器，避免测试依赖外网。
type stubRoundTripper struct {
	handler http.Handler
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := newResponseRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec.result(req)
}

// responseRecorder 是极简的 http.ResponseWriter，只为构造响应。
type responseRecorder struct {
	header http.Header
	body   strings.Builder
	status int
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header), status: http.StatusOK}
}

func (r *responseRecorder) Header() http.Header { return r.header }
func (r *responseRecorder) Write(b []byte) (int, error) {
	return r.body.Write(b)
}
func (r *responseRecorder) WriteHeader(code int) { r.status = code }

func (r *responseRecorder) result(req *http.Request) (*http.Response, error) {
	h := r.header.Clone()
	h.Set("Content-Length", itoa(r.body.Len()))
	return &http.Response{
		StatusCode: r.status,
		Status:     http.StatusText(r.status),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(r.body.String())),
		Request:    req,
	}, nil
}

// withStub 临时替换 HTTPClient，返回恢复函数。
func withStub(t *testing.T, h http.Handler) {
	t.Helper()
	old := HTTPClient
	HTTPClient = &http.Client{Transport: &stubRoundTripper{handler: h}}
	t.Cleanup(func() { HTTPClient = old })
}

// v3Body 是 /v3/config 的最小真实形态：含一个正常模型、一个补全模型、
// 一个图片生成模型（后两个应被 nonChatModel 剔除）。
const v3Body = `{"code":0,"data":{"models":[
 {"id":"glm-5.3","name":"GLM-5.3","maxInputTokens":200000,"maxOutputTokens":131072,
  "supportsImages":true,"supportsReasoning":true,"supportsToolCall":true,"onlyReasoning":true,
  "reasoning":{"defaultEffort":"high","supportedEfforts":["low","high","max"]},
  "descriptionZh":"智谱","tags":["craft"]},
 {"id":"codewise-completions","name":"x","maxOutputTokens":256},
 {"id":"hunyuan-image-alpha","name":"img","tags":["text-to-image"]}
],"productFeaturesConfig":{"ModelTrialBanner":{"banners":[{"modelId":"hy4-preview-f","targetModelId":"glm-5.3"}]}}}}`

// entBody 是企业端点的最小形态：cli agent 列了两个模型，其中一个 /v3 没有。
const entBody = `{"code":0,"data":{
 "agents":[{"name":"cli","models":["glm-5.3","hy3"]},{"name":"other","models":["ignored"]}],
 "models":[{"id":"glm-5.3","name":"GLM-5.3","maxOutputTokens":131072},
           {"id":"hy3","name":"Hy3","maxOutputTokens":64000,
            "supportsReasoning":true,"reasoning":{"supportedEfforts":["low","high"]}}]}}`

// TestFetchCatalogMerge 两路合并：v3 为主、企业端点补缺、非对话模型剔除。
func TestFetchCatalogMerge(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(PathCatalogV3, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("v3 缺 Bearer: %q", got)
		}
		if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "CodeBuddyIDE") && !strings.Contains(ua, "CLI") {
			t.Errorf("v3 的 UA 应含 CodeBuddyIDE，实际 %q", ua)
		}
		_, _ = w.Write([]byte(v3Body))
	})
	mux.HandleFunc(PathCatalogEnterprise, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-User-Id"); got != "u1" {
			t.Errorf("企业端点缺 X-User-Id: %q", got)
		}
		_, _ = w.Write([]byte(entBody))
	})
	withStub(t, mux)

	infos, err := FetchCatalog(context.Background(), "https://copilot.tencent.com", &Account{UID: "u1", Token: "tok"})
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}

	ids := make([]string, 0, len(infos))
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		ids = append(ids, mi.ID)
		byID[mi.ID] = mi
	}
	want := []string{"glm-5.3", "hy4-preview-f", "hy3"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	// 非对话模型不得出现
	for _, bad := range []string{"codewise-completions", "hunyuan-image-alpha"} {
		if _, ok := byID[bad]; ok {
			t.Fatalf("非对话模型 %s 不应出现在目录里", bad)
		}
	}
	// v3 条目字段完整
	glm := byID["glm-5.3"]
	if glm.Name != "GLM-5.3" || !glm.SupportsImages || !glm.SupportsReasoning {
		t.Fatalf("glm-5.3 字段解析不对: %+v", glm)
	}
	if strings.Join(glm.SupportedEfforts, ",") != "low,high,max" || glm.DefaultEffort != "high" {
		t.Fatalf("glm-5.3 档位解析不对: %+v", glm)
	}
	if glm.ContextWindow != 200000 || glm.MaxTokens != 131072 {
		t.Fatalf("glm-5.3 窗口解析不对: %+v", glm)
	}
	// 试用横幅模型继承目标模型能力
	if trial := byID["hy4-preview-f"]; !trial.SupportsImages || len(trial.SupportedEfforts) == 0 {
		t.Fatalf("试用模型未继承能力: %+v", trial)
	}
	// 企业端点补进来的 hy3 保留了自己的字段
	if hy3 := byID["hy3"]; !hy3.SupportsReasoning || hy3.MaxTokens != 64000 {
		t.Fatalf("hy3 解析不对: %+v", hy3)
	}
}

// TestFetchCatalogOneSideFails 一路失败不应拖累另一路。
func TestFetchCatalogOneSideFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(PathCatalogV3, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	mux.HandleFunc(PathCatalogEnterprise, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(entBody))
	})
	withStub(t, mux)

	infos, err := FetchCatalog(context.Background(), "https://copilot.tencent.com", &Account{UID: "u1", Token: "tok"})
	if err != nil {
		t.Fatalf("一路失败时应仍返回结果: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("应拿到企业端点的 2 个模型，实际 %d: %+v", len(infos), infos)
	}
}

// TestFetchCatalogBothFail 两路都失败才报错。
func TestFetchCatalogBothFail(t *testing.T) {
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}
	mux.HandleFunc(PathCatalogV3, fail)
	mux.HandleFunc(PathCatalogEnterprise, fail)
	withStub(t, mux)

	if _, err := FetchCatalog(context.Background(), "https://copilot.tencent.com", &Account{UID: "u1", Token: "tok"}); err == nil {
		t.Fatal("两路都失败时应当报错")
	}
}

// TestNonChatModel 非对话模型的三条剔除规则。
func TestNonChatModel(t *testing.T) {
	cases := []struct {
		id   string
		max  int64
		tags []string
		want bool
	}{
		{"glm-5.3", 131072, nil, false},
		{"codewise-rewrite", 32000, nil, true},
		{"nes-gf", 32000, nil, true},
		{"completion-x", 32000, nil, true},
		{"tiny", 256, nil, true},
		{"img", 32000, []string{"text-to-image"}, true},
		{"vid", 32000, []string{"text-to-video"}, true},
		{"hunyuan-image-alpha", 0, nil, false},
	}
	for _, c := range cases {
		if got := nonChatModel(c.id, c.max, c.tags); got != c.want {
			t.Errorf("nonChatModel(%q,%d,%v) = %v, want %v", c.id, c.max, c.tags, got, c.want)
		}
	}
}

// TestOriginFor 域名兜底：realm 决定默认域，显式 origin 优先且去掉 /v1。
func TestOriginFor(t *testing.T) {
	if got := OriginFor("", &Account{Domain: "copilot.tencent.com"}); got != "https://copilot.tencent.com" {
		t.Fatalf("CN 兜底错: %s", got)
	}
	if got := OriginFor("", &Account{Realm: "global"}); got != "https://www.workbuddy.ai" {
		t.Fatalf("global 兜底错: %s", got)
	}
	if got := OriginFor("https://copilot.tencent.com/v1", nil); got != "https://copilot.tencent.com" {
		t.Fatalf("/v1 未剥离: %s", got)
	}
	if got := OriginFor("https://www.workbuddy.ai/v1/", nil); got != "https://www.workbuddy.ai" {
		t.Fatalf("尾斜杠未处理: %s", got)
	}
}

// TestValidateCatalogURL 未知路径 / 非法协议必须被拒（防止令牌被发到非目录端点）。
//
// 注意：本函数只锁「路径 + 协议」，不锁域名——自建反代需要一个可配置的 origin。
// 域名层如果也要锁，应该由调用方按 expectedHost 校验（见 catalogGet 里 X-Domain 的取值）。
func TestValidateCatalogURL(t *testing.T) {
	ok := []string{
		"https://copilot.tencent.com/v3/config",
		"https://copilot.tencent.com/console/enterprises/personal/models",
	}
	for _, u := range ok {
		if err := ValidateCatalogURL(u, nil); err != nil {
			t.Errorf("%s 应合法: %v", u, err)
		}
	}
	bad := []string{
		"https://copilot.tencent.com/v2/report",              // 非目录端点
		"https://copilot.tencent.com/v3/config/../v2/report", // 绕过尝试
		"ftp://copilot.tencent.com/v3/config",                // 协议非法
		"https://user:pw@copilot.tencent.com/v3/config",      // 不允许带用户信息
	}
	for _, u := range bad {
		if err := ValidateCatalogURL(u, nil); err == nil {
			t.Errorf("%s 应被拒绝", u)
		}
	}
}
