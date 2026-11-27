package gateway

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 面板 JS 的静态自检。
//
// 起因：重构折叠布局时误删了 renderLogToggle 和 loadCompress 两个函数，
// 但调用点还在，运行时报 "xxx is not defined"，
// 而且只有点到那个按钮才会发现。这里在编译期就查出来。

func readPanel(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("读面板失败: %v", err)
	}
	return string(raw)
}

// 所有被调用但从未定义的函数名都要抓出来。
func TestPanelNoUndefinedFunctions(t *testing.T) {
	s := readPanel(t)

	// 已知的内置/浏览器全局，不算未定义
	builtin := map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"function": true, "return": true, "typeof": true, "new": true,
		"parseInt": true, "parseFloat": true, "isNaN": true, "String": true,
		"Number": true, "Boolean": true, "Array": true, "Object": true,
		"JSON": true, "Math": true, "Date": true, "Promise": true,
		"setTimeout": true, "setInterval": true, "clearTimeout": true, "clearInterval": true,
		"fetch": true, "alert": true, "confirm": true, "encodeURIComponent": true,
		"decodeURIComponent": true, "escape": true, "unescape": true,
		"isFinite": true, "Map": true, "Set": true, "RegExp": true, "Error": true,
	}

	// 收集定义：function xxx( 以及 const xxx = / let xxx = / var xxx =
	defRe := regexp.MustCompile(`(?m)(?:function\s+([A-Za-z_$][\w$]*)\s*\(|(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=)`)
	defined := map[string]bool{}
	for _, m := range defRe.FindAllStringSubmatch(s, -1) {
		if m[1] != "" {
			defined[m[1]] = true
		}
		if m[2] != "" {
			defined[m[2]] = true
		}
	}

	// 收集调用：xxx( 但不是 . xxx( 也不是 function/if 等
	callRe := regexp.MustCompile(`(?:^|[^.\w$])([A-Za-z_$][\w$]*)\s*\(`)
	var missing []string
	seen := map[string]bool{}
	for _, m := range callRe.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if builtin[name] || defined[name] || seen[name] {
			continue
		}
		// 关键字与常见语句开头
		if strings.HasPrefix(name, "x") && len(name) <= 2 {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}

	// el() 这类通过字符串取 DOM 的不算函数定义问题，只关心真正的函数调用
	var real []string
	for _, n := range missing {
		switch n {
		case "on", "el", "api", "esc", "toast", "async", "await", "then", "catch",
			"addEventListener", "querySelector", "getElementById", "forEach",
			"map", "filter", "find", "some", "every", "join", "split", "replace",
			"trim", "slice", "splice", "push", "pop", "shift", "unshift",
			"toFixed", "toString", "padStart", "padEnd", "includes", "indexOf",
			"toLocaleString", "toUpperCase", "toLowerCase", "startsWith", "endsWith",
			"createElement", "appendChild", "removeChild", "setAttribute",
			"getAttribute", "classList", "focus", "blur", "click", "preventDefault",
			"stopPropagation", "open", "close", "send", "abort", "resolve", "reject",
			"toISOString", "getTime", "getFullYear", "getMonth", "getDate",
			"getHours", "getMinutes", "getSeconds", "keys", "values", "entries",
			"assign", "stringify", "parse", "from", "of", "all", "race",
			"clear", "delete", "has", "get", "set", "add",
			// CSS / 浏览器 API，不是 JS 函数定义
			"var", "calc", "env", "gradient", "repeat", "media",
			"translateX", "translateY", "rotate", "rgba", "hash",
			"localStorage", "execCommand", "prompt", "Blob":
			continue
		}
		real = append(real, n)
	}

	if len(real) > 0 {
		t.Errorf("面板调用了未定义的函数（重构时容易误删）: %v", real)
	}
}

// 关键调用点必须存在对应定义 —— 覆盖这次踩过的两个坑。
func TestPanelCriticalFunctionsDefined(t *testing.T) {
	s := readPanel(t)
	for _, fn := range []string{
		"renderLogToggle", "loadCompress", "loadQuality", "loadToolUsage",
		"renderToolList", "loadSwitchPolicy", "loadRoutingRules",
		"loadCompress", "saveCompressOnly", "saveTrimOnly",
		"runKeepalive",
	} {
		if !strings.Contains(s, "function "+fn+"(") && !strings.Contains(s, "function "+fn+" (") {
			t.Errorf("缺少函数定义: %s", fn)
		}
	}
}

// 压缩与裁剪必须各有独立保存按钮，不能共用一个。
func TestCompressAndTrimHaveSeparateButtons(t *testing.T) {
	s := readPanel(t)
	if !strings.Contains(s, `id="btnCompressSave"`) {
		t.Error("缺少压缩保存按钮 btnCompressSave")
	}
	if !strings.Contains(s, `id="btnTrimSave"`) {
		t.Error("缺少裁剪保存按钮 btnTrimSave")
	}
}

// Token 保活按钮必须存在，且绑定到 /api/tokens/keepalive。
func TestKeepaliveButtonWired(t *testing.T) {
	s := readPanel(t)
	if !strings.Contains(s, `id="btnKeepalive"`) {
		t.Error("缺少保活按钮 btnKeepalive")
	}
	if !strings.Contains(s, "/api/tokens/keepalive") {
		t.Error("保活按钮没有指向 /api/tokens/keepalive")
	}
}
