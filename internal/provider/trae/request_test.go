package trae

import (
	"encoding/json"
	"strings"
	"testing"
)

// transform 是一个便捷包装：把标准请求体喂进 TransformRequestWith 并解出来看。
func transform(t *testing.T, body, model string, opt Options) map[string]any {
	t.Helper()
	out, err := TransformRequestWith([]byte(body), model, opt)
	if err != nil {
		t.Fatalf("TransformRequestWith: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("结果不是 JSON 对象: %v (%s)", err, out)
	}
	return m
}

func TestBuildChatURL(t *testing.T) {
	want := "https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat"
	cases := []string{
		"",
		"https://trae-api-cn.mchost.guru",
		"https://trae-api-cn.mchost.guru/",
		"https://trae-api-cn.mchost.guru/v1",
		"https://trae-api-cn.mchost.guru/v1/",
		"https://trae-api-cn.mchost.guru/v1/chat/completions",
		"https://trae-api-cn.mchost.guru/chat/completions",
		"https://trae-api-cn.mchost.guru/api/agent/v3",
		"https://trae-api-cn.mchost.guru/api/agent/v3/",
		"https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat",
	}
	for _, in := range cases {
		if got := BuildChatURL(in); got != want {
			t.Errorf("BuildChatURL(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// 基本字段：补齐 session_id/request_id、强制 stream、丢弃 Trae 不认的采样字段。
func TestTransformRequestBasics(t *testing.T) {
	in := `{
		"model":"Doubao-Seed-1.6",
		"messages":[{"role":"user","content":"你好"}],
		"stream":false,
		"top_p":0.9,"stop":["x"],"response_format":{"type":"json_object"},
		"presence_penalty":0.5,"seed":7,"n":1,
		"temperature":0.3,"max_tokens":128
	}`
	m := transform(t, in, "", Options{})

	if m["stream"] != true {
		t.Errorf("stream 必须被强制成 true，实际 %v", m["stream"])
	}
	if m["model"] != "Doubao-Seed-1.6" || m["config_name"] != "Doubao-Seed-1.6" {
		t.Errorf("model/config_name = %v/%v", m["model"], m["config_name"])
	}
	if m["function"] != DefaultFunction {
		t.Errorf("function = %v", m["function"])
	}
	if _, ok := m["model_name"]; ok {
		t.Errorf("没有变体后缀时不该带 model_name")
	}
	sid, _ := m["session_id"].(string)
	rid, _ := m["request_id"].(string)
	if len(sid) != 36 || len(rid) != 36 {
		t.Fatalf("session_id/request_id 不是 uuid: %q %q", sid, rid)
	}
	// 每条请求的 request_id 都要新生成
	m2 := transform(t, in, "", Options{})
	if m2["request_id"] == rid {
		t.Errorf("request_id 应该每条请求都不一样")
	}
	if m["max_tokens"] != float64(128) {
		t.Errorf("max_tokens = %v", m["max_tokens"])
	}
	if m["temperature"] != 0.3 {
		t.Errorf("temperature = %v", m["temperature"])
	}
	for _, dropped := range []string{"top_p", "stop", "response_format", "presence_penalty", "seed", "n"} {
		if _, ok := m[dropped]; ok {
			t.Errorf("Trae 不认的字段 %q 应该被丢弃", dropped)
		}
	}

	// 字符串内容必须变成 parts 数组
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 长度 = %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	parts, _ := first["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("content parts = %v", first["content"])
	}
	part, _ := parts[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "你好" {
		t.Errorf("part = %v", part)
	}
}

// 声明了工具时：parameters 变字符串、tool_choice 是对象取名字、历史调用按原生形状发。
func TestTransformRequestNativeTools(t *testing.T) {
	in := `{
		"model":"m",
		"messages":[
			{"role":"system","content":"你是助手"},
			{"role":"user","content":"天气"},
			{"role":"assistant","content":"","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"潮州\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"晴"}
		],
		"tools":[{"type":"function","function":{
			"name":"get_weather","description":"查天气",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}
		}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}},
		"parallel_tool_calls":true,
		"reasoning_effort":"low"
	}`
	m := transform(t, in, "", Options{})

	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", m["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	params, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters 必须是 JSON 字符串，实际 %T", fn["parameters"])
	}
	if !isJSONObject(params) || !strings.Contains(params, `"city"`) {
		t.Errorf("parameters = %q", params)
	}
	if m["tool_choice"] != "get_weather" {
		t.Errorf("tool_choice = %v", m["tool_choice"])
	}
	if m["parallel_tool_calls"] != true {
		t.Errorf("parallel_tool_calls = %v", m["parallel_tool_calls"])
	}
	if m["reasoning_effort"] != "light" {
		t.Errorf("reasoning_effort = %v（low 应映射成 light）", m["reasoning_effort"])
	}

	msgs, _ := m["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages 长度 = %d", len(msgs))
	}
	assistant, _ := msgs[2].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v", assistant["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	inner, _ := call["function_call"].(map[string]any)
	if inner["name"] != "get_weather" || inner["arguments"] != `{"city":"潮州"}` {
		t.Errorf("function_call = %v", inner)
	}
	if assistant["content"] != nil {
		t.Errorf("没有正文的助手消息 content 应为 null，实际 %v", assistant["content"])
	}
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
		t.Errorf("工具结果消息 = %v", toolMsg)
	}
}

// tool_choice=none：不下发工具，历史调用与结果都改写成正文。
func TestTransformRequestToolChoiceNone(t *testing.T) {
	in := `{
		"model":"m",
		"messages":[
			{"role":"user","content":"天气"},
			{"role":"assistant","content":"","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"潮州\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"晴"}
		],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],
		"tool_choice":"none"
	}`
	m := transform(t, in, "", Options{})
	if _, ok := m["tools"]; ok {
		t.Errorf("tool_choice=none 时不该下发 tools")
	}
	if _, ok := m["tool_choice"]; ok {
		t.Errorf("tool_choice=none 时不该下发 tool_choice")
	}

	msgs, _ := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages 长度 = %d", len(msgs))
	}
	assistant, _ := msgs[1].(map[string]any)
	parts, _ := assistant["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("assistant content = %v", assistant["content"])
	}
	text, _ := parts[0].(map[string]any)
	s, _ := text["text"].(string)
	if !strings.HasPrefix(s, "<tool_call>") || !strings.Contains(s, "get_weather") {
		t.Errorf("历史调用应写成 <tool_call> 文本，实际 %q", s)
	}

	result, _ := msgs[2].(map[string]any)
	if result["role"] != "user" {
		t.Fatalf("没有原生调用轮次时，工具结果应改写成 user 消息，实际 %v", result["role"])
	}
	rp, _ := result["content"].([]any)
	rt, _ := rp[0].(map[string]any)
	rs, _ := rt["text"].(string)
	if !strings.Contains(rs, "Result of get_weather (call call_1):\n晴") {
		t.Errorf("工具结果正文 = %q", rs)
	}
}

// model 写成 xxx__max 时拆成 config_name + model_name。
func TestTransformRequestModelVariant(t *testing.T) {
	in := `{"model":"Doubao-Seed-1.6__max","messages":[{"role":"user","content":"hi"}]}`
	m := transform(t, in, "", Options{})
	if m["config_name"] != "Doubao-Seed-1.6" {
		t.Errorf("config_name = %v", m["config_name"])
	}
	if m["model"] != "Doubao-Seed-1.6" {
		t.Errorf("model = %v", m["model"])
	}
	if m["model_name"] != "Doubao-Seed-1.6__max" {
		t.Errorf("model_name = %v", m["model_name"])
	}
}

// user_message_context：知道上下文窗口时才带，值是「窗口 - 本条输出上限」。
func TestTransformRequestContextWindow(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`
	m := transform(t, in, "", Options{PromptMaxTokens: 200000})
	ctx, _ := m["user_message_context"].(map[string]any)
	info, _ := ctx["model_info"].(map[string]any)
	if info["prompt_max_tokens"] != float64(199000) {
		t.Errorf("prompt_max_tokens = %v", info["prompt_max_tokens"])
	}

	// 不知道窗口就不带
	plain := transform(t, in, "", Options{})
	if _, ok := plain["user_message_context"]; ok {
		t.Errorf("没有上下文窗口信息时不该带 user_message_context")
	}
}

// 连续同角色消息要合并成一条。
func TestTransformRequestMergeSameRole(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"user","content":"第一句"},
		{"role":"user","content":"第二句"},
		{"role":"assistant","content":"好的"}
	]}`
	m := transform(t, in, "", Options{})
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 长度 = %d（连续 user 应合并）", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	parts, _ := first["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("合并后 parts = %v", first["content"])
	}
}

// developer 角色要降级成 system；不认识的角色报错。
func TestTransformRequestRoles(t *testing.T) {
	m := transform(t, `{"model":"m","messages":[{"role":"developer","content":"规则"}]}`, "", Options{})
	msgs, _ := m["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("developer 应降级为 system，实际 %v", first["role"])
	}
	if _, err := TransformRequest([]byte(`{"model":"m","messages":[{"role":"robot","content":"x"}]}`), ""); err == nil {
		t.Errorf("未知角色应该报错")
	}
	if _, err := TransformRequest([]byte(`{"model":"m"}`), ""); err == nil {
		t.Errorf("缺 messages 应该报错")
	}
}

// ToolSpecs 用来告诉流转换「本次声明了哪些工具」。
func TestToolSpecs(t *testing.T) {
	body := `{"model":"m","tools":[
		{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}},
		{"type":"function","function":{"name":"get_time"}}
	]}`
	specs := ToolSpecs([]byte(body))
	if len(specs) != 2 || specs[0].Name != "get_weather" || specs[1].Name != "get_time" {
		t.Fatalf("ToolSpecs = %+v", specs)
	}
	if string(specs[1].Parameters) != "" {
		t.Errorf("没有 parameters 时应该留空，实际 %s", specs[1].Parameters)
	}

	none := `{"model":"m","tool_choice":"none","tools":[{"type":"function","function":{"name":"get_weather"}}]}`
	if got := ToolSpecs([]byte(none)); got != nil {
		t.Errorf("tool_choice=none 时不该算声明工具，实际 %+v", got)
	}
}
