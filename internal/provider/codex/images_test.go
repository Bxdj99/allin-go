package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestIsImageModel(t *testing.T) {
	cases := map[string]bool{
		"gpt-image-2":       true,
		"codex/gpt-image-2": true,
		"gpt-image-1":       true,
		"gpt-image-2-hd":    true,
		"gpt-4o":            false,
		"gpt-image-":        false,
		"gpt-image-2/../x":  false,
		"":                  false,
		"dall-e-3":          false,
	}
	for m, want := range cases {
		if got := IsImageModel(m); got != want {
			t.Errorf("IsImageModel(%q)=%v want %v", m, got, want)
		}
	}
}

func TestBuildImagePayload(t *testing.T) {
	raw := []byte(`{"model":"codex/gpt-image-2","prompt":"一只橘猫","n":2,"size":"1024x1024","quality":"high","unknown":"drop"}`)
	out, err := BuildImagePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["model"] != "gpt-image-2" {
		t.Errorf("model 前缀应去掉，得到 %v", m["model"])
	}
	if m["prompt"] != "一只橘猫" {
		t.Errorf("prompt 不应丢，得到 %v", m["prompt"])
	}
	if m["n"] != float64(2) {
		t.Errorf("n 应为 2，得到 %v", m["n"])
	}
	if _, ok := m["unknown"]; ok {
		t.Error("未知字段应被丢弃")
	}
}

func TestBuildImagePayloadDefaultModel(t *testing.T) {
	out, err := BuildImagePayload([]byte(`{"prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["model"] != ImagesModel {
		t.Errorf("缺 model 时应补默认值，得到 %v", m["model"])
	}
	if m["n"] != float64(1) {
		t.Errorf("n 默认应为 1，得到 %v", m["n"])
	}
}

func TestBuildImagePayloadPromptParts(t *testing.T) {
	raw := []byte(`{"prompt":[{"type":"text","text":"画"},{"type":"text","text":"只猫"}]}`)
	out, err := BuildImagePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["prompt"] != "画只猫" {
		t.Errorf("分段 prompt 应拼接，得到 %v", m["prompt"])
	}
}

func TestBuildImagePayloadErrors(t *testing.T) {
	if _, err := BuildImagePayload([]byte(`{"prompt":""}`)); err == nil {
		t.Error("空 prompt 应报错")
	}
	if _, err := BuildImagePayload([]byte(`{"prompt":"x","n":9}`)); err == nil {
		t.Error("n 超限应报错")
	}
	if _, err := BuildImagePayload([]byte(`not json`)); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// API key 形态不能生图，要给出明确错误而不是打到上游。
func TestGenerateImageRejectsAPIKey(t *testing.T) {
	_, err := GenerateImage(context.Background(), ImageAccount{Access: "sk-abc"}, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "登录态") {
		t.Fatalf("应提示需要登录态账号，实际 %v", err)
	}
}

func TestGenerateImageRequiresAccess(t *testing.T) {
	if _, err := GenerateImage(context.Background(), ImageAccount{}, []byte(`{}`)); err == nil {
		t.Fatal("缺 access 应报错")
	}
}

func TestNewUUIDShape(t *testing.T) {
	u := newUUID()
	parts := strings.Split(u, "-")
	if len(parts) != 5 {
		t.Fatalf("UUID 段数不对: %s", u)
	}
	if len(parts[0]) != 8 || len(parts[4]) != 12 {
		t.Fatalf("UUID 长度不对: %s", u)
	}
	if u == newUUID() {
		t.Fatal("两次生成不应相同")
	}
}
