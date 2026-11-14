package gateway

import (
	"reflect"
	"testing"

	"allin/internal/config"
)

// TestMergeModelsAppend 只追加：用户手填的保留、上游新增的追加、顺序稳定。
func TestMergeModelsAppend(t *testing.T) {
	cur := []string{"my-alias", "glm-5.3"}
	fetched := []string{"glm-5.3", "hy3", "deepseek-v4.1-flash", "hy3"}

	merged, added, kept := mergeModels(cur, fetched)

	wantMerged := []string{"my-alias", "glm-5.3", "hy3", "deepseek-v4.1-flash"}
	if !reflect.DeepEqual(merged, wantMerged) {
		t.Fatalf("merged = %v, want %v", merged, wantMerged)
	}
	wantAdded := []string{"hy3", "deepseek-v4.1-flash"}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("added = %v, want %v", added, wantAdded)
	}
	// my-alias 上游没下发但必须保留；glm-5.3 上游下发了，不算 kept。
	wantKept := []string{"my-alias"}
	if !reflect.DeepEqual(kept, wantKept) {
		t.Fatalf("kept = %v, want %v", kept, wantKept)
	}
}

// TestMergeModelsNoChange 用户列表已包含上游全部模型时不产生变更。
func TestMergeModelsNoChange(t *testing.T) {
	cur := []string{"a", "b"}
	merged, added, kept := mergeModels(cur, []string{"b", "a"})
	if len(added) != 0 {
		t.Fatalf("added 应为空，实际 %v", added)
	}
	if len(kept) != 0 {
		t.Fatalf("kept 应为空，实际 %v", kept)
	}
	if !reflect.DeepEqual(merged, cur) {
		t.Fatalf("merged = %v, want %v", merged, cur)
	}
}

// TestMergeModelsEmptyFetched 上游没下发时不能清空用户的列表。
func TestMergeModelsEmptyFetched(t *testing.T) {
	cur := []string{"a", "b"}
	merged, added, _ := mergeModels(cur, nil)
	if len(added) != 0 {
		t.Fatalf("added 应为空，实际 %v", added)
	}
	if !reflect.DeepEqual(merged, cur) {
		t.Fatalf("merged = %v, want %v", merged, cur)
	}
}

// TestMergeModelsDedupAndBlank 空白项与重复项要清掉。
func TestMergeModelsDedupAndBlank(t *testing.T) {
	merged, _, _ := mergeModels([]string{"a", "", "a"}, []string{" ", "b", "b"})
	if !reflect.DeepEqual(merged, []string{"a", "b"}) {
		t.Fatalf("merged = %v", merged)
	}
}

// TestSyncTargets unknown providerId 要报错，已知的要能取到。
func TestSyncTargets(t *testing.T) {
	s := &Server{}
	cfg := &config.Config{Providers: []*config.Provider{{ID: "wb", Kind: "workbuddy"}}}
	if _, err := s.syncTargets(cfg, "nope"); err == nil {
		t.Fatal("未知 providerId 应当报错")
	}
	got, err := s.syncTargets(cfg, "wb")
	if err != nil || len(got) != 1 || got[0].ID != "wb" {
		t.Fatalf("按 id 取供应商失败: got=%v err=%v", got, err)
	}
}
