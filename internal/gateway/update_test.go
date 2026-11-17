package gateway

import "testing"

// 这是实际踩到的回归：本地跑 v1.18.0（未发 Release），GitHub 最新 Release 还是
// v1.17.1，只判「不相等」会报成有更新，用户一点就降级，丢掉本地版本的功能。
func TestCompareVerLocalNewerThanRelease(t *testing.T) {
	cmp, ok := compareVer("v1.17.1", "v1.18.0-go")
	if !ok {
		t.Fatal("应能解析两个版本号")
	}
	if cmp > 0 {
		t.Fatal("v1.17.1 不该被判为比 v1.18.0 新（会导致误报更新并降级）")
	}
	if cmp != -1 {
		t.Fatalf("期望 -1，实际 %d", cmp)
	}
}

func TestCompareVerBasic(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.18.0", "v1.17.1", 1},
		{"v1.17.1", "v1.18.0", -1},
		{"v1.18.0", "v1.18.0", 0},
		{"v1.18.0-go", "1.18.0", 0},
		{"1.18", "1.18.0", 0},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.10.0", "v1.9.9", 1}, // 数字比较，不是字符串比较
		{"v1.18.0-beta", "v1.18.0", 0},
	}
	for _, c := range cases {
		got, ok := compareVer(c.a, c.b)
		if !ok {
			t.Fatalf("compareVer(%q,%q) 解析失败", c.a, c.b)
		}
		if got != c.want {
			t.Errorf("compareVer(%q,%q) = %d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareVerUnparsable(t *testing.T) {
	if _, ok := compareVer("dev", "v1.18.0"); ok {
		t.Error("dev 应解析失败，让调用方退回字符串比较")
	}
	if _, ok := compareVer("", "v1.18.0"); ok {
		t.Error("空版本应解析失败")
	}
}

// 版本号相等时不应报更新。
func TestCompareVerEqualNoUpdate(t *testing.T) {
	cmp, ok := compareVer("v1.18.0", "v1.18.0-go")
	if !ok || cmp != 0 {
		t.Fatalf("应相等，got cmp=%d ok=%v", cmp, ok)
	}
}
