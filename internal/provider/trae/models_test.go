package trae

import (
	"encoding/json"
	"testing"
)

// 真实响应形状（batch_get_detail_param 与 get_detail_param 同构）：
// function_configs[].config_info_list[].config_name 才是模型名。
const sampleBatch = `{
  "code": 0,
  "msg": "",
  "function_configs": [
    {
      "function": "chat_v3",
      "config_info_list": [
        {"config_name": "doubao-seed-1.6", "model_name": "doubao-seed-1.6", "display_name": "Doubao Seed 1.6"},
        {"config_name": "kimi-k2-0905", "model_name": "kimi-k2-0905", "display_name": ""},
        {"config_name": "", "model_name": "ignored", "display_name": ""}
      ]
    },
    {
      "function": "solo_coder",
      "config_info_list": [
        {"config_name": "doubao-seed-1.6", "model_name": "doubao-seed-1.6", "display_name": "dup"},
        {"config_name": "deepseek-v3.1", "model_name": "deepseek-v3.1", "display_name": "DeepSeek V3.1"}
      ]
    }
  ]
}`

const sampleWrapped = `{"code":0,"data":{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"glm-4.6"}]}]}}`

func TestParseFunctionConfigs_Flat(t *testing.T) {
	got, err := parseFunctionConfigs([]byte(sampleBatch))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := []string{"deepseek-v3.1", "doubao-seed-1.6", "kimi-k2-0905"}
	if len(got) != len(want) {
		t.Fatalf("数量不对: got %d %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("第 %d 项 = %q, want %q (all=%v)", i, got[i].Name, want[i], got)
		}
	}
	for _, e := range got {
		if e.Name == "kimi-k2-0905" && e.Note != "kimi-k2-0905" {
			t.Fatalf("note 回落失败: %q", e.Note)
		}
	}
}

func TestParseFunctionConfigs_Wrapped(t *testing.T) {
	got, err := parseFunctionConfigs([]byte(sampleWrapped))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 1 || got[0].Name != "glm-4.6" {
		t.Fatalf("data 包装解析失败: %v", got)
	}
}

func TestParseFunctionConfigs_ErrorCode(t *testing.T) {
	if _, err := parseFunctionConfigs([]byte(`{"code":10001,"msg":"token expired"}`)); err == nil {
		t.Fatal("上游错误码应当返回 error")
	}
}

func TestParseFunctionConfigs_Empty(t *testing.T) {
	if _, err := parseFunctionConfigs([]byte(`{"code":0}`)); err != nil {
		t.Fatalf("空列表不该报解析错误: %v", err)
	}
}

func TestModelEntryJSON(t *testing.T) {
	b, _ := json.Marshal(ModelEntry{Name: "doubao-seed-1.6", Function: "chat_v3"})
	if string(b) != `{"name":"doubao-seed-1.6","function":"chat_v3"}` {
		t.Fatalf("序列化变了: %s", b)
	}
}
