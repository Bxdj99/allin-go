package gateway

import (
	"net/http"

	"allin/internal/config"
)

// 端口迁移提示。
//
// 背景：v1.10.0 把默认端口从 3425 改成 6667（避开 Magpie 等同类网关）。
// 但已有配置里的端口不会被自动改，于是出现「文档说 6667，实际还是 3425」的落差，
// 用户按新文档配客户端就连不上。
//
// 这里提供一个显式提示：启动后告诉用户「你现在用的端口和默认值不一样」，
// 让他自己决定是保持不动还是切过去。不自动改 —— 因为切端口会让已配好的客户端失联。

// DefaultPort 是当前版本的默认端口。
const DefaultPort = 6868

// LegacyPort 是历史默认端口。
const LegacyPort = 3425

// PortNotice 是端口迁移提示。
type PortNotice struct {
	// Show 是否值得提示
	Show bool `json:"show"`
	// Current 当前生效端口
	Current int `json:"current"`
	// Suggested 建议端口
	Suggested int `json:"suggested"`
	// Severity: error（浏览器打不开，必须换）/ warn（建议换）
	Severity string `json:"severity,omitempty"`
	// Reason 为什么建议
	Reason string `json:"reason,omitempty"`
	// Endpoint 当前端口对应的客户端地址
	Endpoint string `json:"endpoint"`
	// SuggestedEndpoint 建议端口对应的客户端地址
	SuggestedEndpoint string `json:"suggestedEndpoint"`
}

// buildPortNotice 判断要不要提示端口。
//
// 两种情况值得提示：
//  1. 当前端口在浏览器受限列表里（最严重：网关好着，但浏览器打不开）
//  2. 当前是历史默认值 3425（新版默认变了，但客户端可能还配着老的）
func buildPortNotice(cfg *config.Config) PortNotice {
	cur := cfg.Server.Port
	n := PortNotice{Current: cur, Suggested: DefaultPort}
	n.Endpoint = endpointFromHost(cfg.Server.Host, cur)
	n.SuggestedEndpoint = endpointFromHost(cfg.Server.Host, DefaultPort)

	if config.IsBrowserBlockedPort(cur) {
		n.Show = true
		n.Severity = "error"
		n.Reason = "当前端口 " + itoaPort(cur) + " 在 Chromium 的受限端口列表里" +
			"（6665-6669 属于 IRC 段），浏览器访问会直接报 ERROR_PORT_BLOCKED。" +
			"网关本身是正常的，点下面的按钮换到 " + itoaPort(DefaultPort) + " 就能打开面板。"
		return n
	}
	if cur == LegacyPort {
		n.Show = true
		n.Severity = "warn"
		n.Reason = "新版默认端口已改为 " + itoaPort(DefaultPort) +
			"（避开 Magpie 等同类网关占用的 " + itoaPort(LegacyPort) + "），" +
			"你当前仍在用 " + itoaPort(LegacyPort) + "。如果客户端已经配的是旧端口，可以保持不动；" +
			"想跟新版默认值一致就切过去，切完记得改客户端地址。"
		return n
	}
	return n
}

func endpointFromHost(host string, port int) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + itoaPort(port) + "/v1"
}

func itoaPort(n int) string {
	if n <= 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// handlePortNotice 返回端口迁移提示（只读）。
//
//	GET /api/port/notice
func (s *Server) handlePortNotice(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	writeJSON(w, http.StatusOK, buildPortNotice(cfg))
}

// handlePortSwitch 把端口切到建议值。
//
//	POST /api/port/switch  {"port": 6667}   不传就用 DefaultPort
//
// 只改配置，需要重启网关生效 —— 返回值里明确说明。
func (s *Server) handlePortSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var in struct {
		Port int `json:"port"`
	}
	_ = decodeJSON(r, &in)
	target := in.Port
	if target == 0 {
		target = DefaultPort
	}
	if target < 1 || target > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "端口超出范围"})
		return
	}
	cfg := s.store.Snapshot()
	old := cfg.Server.Port
	cfg.Server.Port = target
	if err := s.store.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"from":       old,
		"to":         target,
		"endpoint":   endpointFromHost(cfg.Server.Host, target),
		"restart":    true,
		"message":    "端口已写入配置，" + itoaPort(target) + " 要重启网关后才生效。重启后客户端地址改为新端口。",
	})
}
