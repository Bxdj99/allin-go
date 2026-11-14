package provider

import (
	"net/http"
	"strings"
	"testing"

	"allin/internal/config"
	"allin/internal/provider/deepseekweb"
)

// TestBuildRequestDeepSeekWeb DeepSeek 网页版的路径必须被钉到 completion 端点，
// 并且带齐网页前端指纹头——这两点任何一处不对，上游都会当第三方调用拒掉。
func TestBuildRequestDeepSeekWeb(t *testing.T) {
	body := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`
	acct := &config.Account{ID: "a1", Access: "tok-web"}
	req, err := BuildRequest(KindDeepSeekW, "https://chat.deepseek.com", "/chat/completions", acct.Key(), acct, []byte(body), nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %s", req.Method)
	}
	if want := "https://chat.deepseek.com" + deepseekweb.PathCompletion; req.URL.String() != want {
		t.Errorf("url = %s，期望 %s", req.URL, want)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-web" {
		t.Errorf("authorization = %q", got)
	}
	for _, k := range []string{"X-Client-Platform", "X-Client-Version", "X-Client-Bundle-Id", "X-Client-Locale"} {
		if req.Header.Get(k) == "" {
			t.Errorf("缺少指纹头 %s", k)
		}
	}
	if got := req.Header.Get("Referer"); got != deepseekweb.Origin+"/a/chat/" {
		t.Errorf("referer = %q", got)
	}
	// baseUrl 误填成 https://chat.deepseek.com/v1 时也要能被纠正
	req, err = BuildRequest(KindDeepSeekW, "https://chat.deepseek.com/v1", "/chat/completions", "tok-web", acct, []byte(body), nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if strings.Contains(req.URL.Path, "/v1") {
		t.Errorf("baseUrl 的 /v1 没有去掉: %s", req.URL)
	}
}

// TestDefaultBaseURLDeepSeekWeb 没填 baseUrl 时要兜底到网页版源站。
func TestDefaultBaseURLDeepSeekWeb(t *testing.T) {
	if got := DefaultBaseURL(KindDeepSeekW); got != deepseekweb.Origin {
		t.Errorf("DefaultBaseURL = %q，期望 %q", got, deepseekweb.Origin)
	}
}
