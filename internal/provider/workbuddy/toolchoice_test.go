package workbuddy

import (
	"encoding/json"
	"testing"
)

// 回归：字符串形态的 tool_choice="auto" 曾被当成未知值删除，
// 导致模型失去「可以调用工具」的提示，用户感觉变笨。
func TestToolChoiceAutoStringKept(t *testing.T) {
	in := `{"model":"m","tool_choice":"auto","tools":[{"type":"function","function":{"name":"read_file"}}]}`
	out, err := Prepare([]byte(in), "")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	if _, ok := m["tool_choice"]; !ok {
		t.Fatal(`tool_choice="auto" 被删掉了`)
	}
	var v string
	json.Unmarshal(m["tool_choice"], &v)
	if v != "auto" {
		t.Fatalf("tool_choice 应为 auto，实际 %q", v)
	}
}

func TestToolChoiceRequiredStringKept(t *testing.T) {
	in := `{"model":"m","tool_choice":"required","tools":[{"type":"function","function":{"name":"x"}}]}`
	out, _ := Prepare([]byte(in), "")
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	var v string
	if err := json.Unmarshal(m["tool_choice"], &v); err != nil || v != "required" {
		t.Fatalf("required 应保留，实际 %s", m["tool_choice"])
	}
}

// none 仍应连 tools 一起删
func TestToolChoiceNoneRemovesTools(t *testing.T) {
	in := `{"model":"m","tool_choice":"none","tools":[{"type":"function","function":{"name":"x"}}]}`
	out, _ := Prepare([]byte(in), "")
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	if _, ok := m["tool_choice"]; ok {
		t.Error("none 应删除 tool_choice")
	}
	if _, ok := m["tools"]; ok {
		t.Error("none 应连同 tools 一起删除")
	}
}

// 对象形态 auto 仍正常
func TestToolChoiceAutoObject(t *testing.T) {
	in := `{"model":"m","tool_choice":{"type":"auto"},"tools":[{"type":"function","function":{"name":"x"}}]}`
	out, _ := Prepare([]byte(in), "")
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	var v string
	if err := json.Unmarshal(m["tool_choice"], &v); err != nil || v != "auto" {
		t.Fatalf("对象形态 auto 应转成字符串 auto，实际 %s", m["tool_choice"])
	}
}

// 指定具体工具名应转成 function 形态，保留强制调用语义
func TestToolChoiceByName(t *testing.T) {
	in := `{"model":"m","tool_choice":"read_file","tools":[{"type":"function","function":{"name":"read_file"}}]}`
	out, _ := Prepare([]byte(in), "")
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(m["tool_choice"], &obj); err != nil {
		t.Fatalf("应转成对象形态，实际 %s", m["tool_choice"])
	}
	if obj.Type != "function" || obj.Function.Name != "read_file" {
		t.Fatalf("转换结果不对: %+v", obj)
	}
}
