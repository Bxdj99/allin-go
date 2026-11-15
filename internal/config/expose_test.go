package config

import "testing"

// 验证 ExposedModels 的三种语义：空=全暴露、非空=子集、哨兵/失效项=被过滤。
func TestProviderExposed(t *testing.T) {
	p := &Provider{ID: "x", Models: []string{"a", "b", "c"}}

	if got := p.Exposed(); len(got) != 3 {
		t.Fatalf("空 exposedModels 应全暴露，得到 %v", got)
	}
	if !p.IsModelExposed("b") {
		t.Fatal("空 exposedModels 时 b 应暴露")
	}

	p.ExposedModels = []string{"a", "c"}
	if got := p.Exposed(); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("子集结果不对: %v", got)
	}
	if p.IsModelExposed("b") {
		t.Fatal("b 没被勾选，不该暴露")
	}
	// 顺序以 provider 为准，再按用户勾选过滤
	p.ExposedModels = []string{"c", "a"}
	if got := p.Exposed(); got[0] != "a" || got[1] != "c" {
		t.Fatalf("应按 models 顺序输出: %v", got)
	}

	// 指向不存在的模型（含哨兵）应被过滤，不产生幽灵条目
	p.ExposedModels = []string{"a", "\x00none", "gone"}
	if got := p.Exposed(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("失效项应被过滤: %v", got)
	}

	// 全部不存在 = 一个都不暴露
	p.ExposedModels = []string{"\x00none"}
	if got := p.Exposed(); len(got) != 0 {
		t.Fatalf("应为空列表: %v", got)
	}
}

// 空模型的 provider 上判断不应 panic。
func TestProviderExposedNil(t *testing.T) {
	var p *Provider
	if p.Exposed() != nil || p.IsModelExposed("a") {
		t.Fatal("nil provider 应返回空")
	}
}
