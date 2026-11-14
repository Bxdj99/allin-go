// Package prompt 负责「系统提示词注入」。
//
// 为什么放在网关层：客户端（各种套壳 App、IDE 插件）自己可能带一套拒答型系统提示词，
// 也可能根本没有。只要在网关这里统一前置/覆盖 system 消息，客户端怎么改都绕不过去。
//
// 支持两种输入协议：
//   - OpenAI：messages 数组里的 {"role":"system"} 那条
//   - Anthropic：顶层 "system" 字段（可以是字符串，也可以是 block 数组）
package prompt

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Mode 是注入策略。
const (
	ModePassthrough = "passthrough" // 原样转发（默认）
	ModeInject      = "inject"      // 强制注入
	ModeReplace     = "replace"     // 替换客户端自带的 system
)

// Config 是提示词配置（与 Java 版的 prompt 字段兼容并扩展）。
type Config struct {
	Mode   string `json:"mode,omitempty"`
	File   string `json:"file,omitempty"`
	Inline string `json:"inline,omitempty"`
}

// Load 返回要注入的提示词正文。File 优先，其次 Inline。
func (c Config) Load() (string, error) {
	if c.Mode != ModeInject && c.Mode != ModeReplace {
		return "", nil
	}
	if c.File != "" {
		raw, err := os.ReadFile(c.File)
		if err != nil {
			return "", fmt.Errorf("读取提示词文件失败: %w", err)
		}
		text := strings.TrimSpace(string(raw))
		if text == "" {
			return "", fmt.Errorf("提示词文件是空的: %s", c.File)
		}
		return text, nil
	}
	text := strings.TrimSpace(c.Inline)
	if text == "" {
		return "", fmt.Errorf("prompt.mode=%s 但既没有 file 也没有 inline", c.Mode)
	}
	return text, nil
}

// ApplyOpenAI 把提示词塞进 OpenAI 形态的请求体。
//
// mode=replace：删掉客户端所有 system/developer 消息，只留我们这条；
// mode=inject：保留客户端的，我们的放在最前面（优先级更高）。
func ApplyOpenAI(body []byte, text, mode string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	var msgs []map[string]json.RawMessage
	if raw, ok := m["messages"]; ok {
		_ = json.Unmarshal(raw, &msgs)
	}

	out := make([]map[string]json.RawMessage, 0, len(msgs)+1)
	// 我们的系统提示放最前
	content, _ := json.Marshal(text)
	role, _ := json.Marshal("system")
	sys := map[string]json.RawMessage{"role": role, "content": content}
	out = append(out, sys)

	for _, msg := range msgs {
		if mode == ModeReplace {
			var r string
			_ = json.Unmarshal(msg["role"], &r)
			if r == "system" || r == "developer" {
				continue // 丢弃客户端自带的
			}
		}
		out = append(out, msg)
	}

	buf, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	m["messages"] = buf
	return json.Marshal(m)
}

// ApplyAnthropic 把提示词塞进 Anthropic 形态的请求体（顶层 system 字段）。
func ApplyAnthropic(body []byte, text, mode string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	var existing string
	if raw, ok := m["system"]; ok {
		// system 可能是字符串，也可能是 block 数组
		if json.Unmarshal(raw, &existing) != nil {
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(raw, &blocks) == nil {
				var sb strings.Builder
				for _, b := range blocks {
					var t, typ string
					_ = json.Unmarshal(b["text"], &t)
					_ = json.Unmarshal(b["type"], &typ)
					if typ == "text" || typ == "" {
						sb.WriteString(t)
					}
				}
				existing = sb.String()
			}
		}
	}

	final := text
	if mode != ModeReplace && strings.TrimSpace(existing) != "" {
		// 客户端自带的放后面，我们的在前
		final = text + "\n\n" + existing
	}
	buf, _ := json.Marshal(final)
	m["system"] = buf
	return json.Marshal(m)
}
