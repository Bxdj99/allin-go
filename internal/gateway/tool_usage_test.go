package gateway

import "testing"

// 工具使用统计：找出「声明了但从没用过」的工具 —— 那是纯成本。
// 实测用户 182 个工具占单次输入 35%，而历史里真正用到的只有个位数。

func TestExtractToolNamesOpenAI(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"type":"function","function":{"name":"terminal"}},
			{"type":"function","function":{"name":"read_file"}},
			{"type":"function","function":{"name":"never_used"}}
		],
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"1","function":{"name":"terminal","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"1","content":"ok"}
		]
	}`)

	declared, used := extractToolNames(body)
	if len(declared) != 3 {
		t.Errorf("应声明 3 个工具，得到 %d", len(declared))
	}
	if len(used) != 1 || used[0] != "terminal" {
		t.Errorf("应只用到 terminal，得到 %v", used)
	}
}

// Anthropic 形态：content 里 type==tool_use。
func TestExtractToolNamesAnthropic(t *testing.T) {
	body := []byte(`{
		"tools":[{"name":"bash","description":"run"}],
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"我来跑一下"},
				{"type":"tool_use","id":"t1","name":"bash","input":{}}
			]}
		]
	}`)

	declared, used := extractToolNames(body)
	if len(declared) != 1 || declared[0] != "bash" {
		t.Errorf("应声明 bash，得到 %v", declared)
	}
	if len(used) != 1 || used[0] != "bash" {
		t.Errorf("应用到 bash，得到 %v", used)
	}
}

// 去重：同一工具在多个消息里用过，只记一次集合。
func TestExtractToolNamesDedup(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"function":{"name":"a"}}]},
			{"role":"assistant","tool_calls":[{"function":{"name":"a"}}]},
			{"role":"assistant","tool_calls":[{"function":{"name":"b"}}]}
		]
	}`)
	_, used := extractToolNames(body)
	if len(used) != 2 {
		t.Errorf("应去重为 2 个，得到 %v", used)
	}
}

// 空/坏请求不能 panic。
func TestExtractToolNamesInvalid(t *testing.T) {
	for _, b := range [][]byte{nil, []byte{}, []byte("not json"), []byte(`{}`)} {
		d, u := extractToolNames(b)
		if len(d) != 0 || len(u) != 0 {
			t.Errorf("无效输入应返回空，得到 %d/%d", len(d), len(u))
		}
	}
}

// 统计后能正确标出 idle（声明过但没用过）。
func TestToolUsageIdleDetection(t *testing.T) {
	// 重置全局状态，避免和其他测试串味
	toolUsageMu.Lock()
	toolUsageTable = map[string]*toolUsageStat{}
	toolUsageMu.Unlock()

	body := []byte(`{
		"tools":[
			{"type":"function","function":{"name":"used_tool"}},
			{"type":"function","function":{"name":"idle_tool"}}
		],
		"messages":[
			{"role":"assistant","tool_calls":[{"function":{"name":"used_tool"}}]}
		]
	}`)
	recordToolTraffic(body)

	rows := toolUsageRows()
	if len(rows) != 2 {
		t.Fatalf("应有 2 行，得到 %d", len(rows))
	}
	// idle 的排前面
	if rows[0].Name != "idle_tool" || !rows[0].Idle {
		t.Errorf("idle_tool 应排在最前且标记 idle，实际 %+v", rows[0])
	}
	if rows[1].Name != "used_tool" || rows[1].Idle {
		t.Errorf("used_tool 不该标记 idle，实际 %+v", rows[1])
	}

	sum := toolUsageSummaryOf()
	if sum.Total != 2 || sum.UsedEver != 1 || sum.Idle != 1 {
		t.Errorf("汇总不符：%+v", sum)
	}
}

// 多次请求应累计声明次数。
func TestToolUsageAccumulates(t *testing.T) {
	toolUsageMu.Lock()
	toolUsageTable = map[string]*toolUsageStat{}
	toolUsageMu.Unlock()

	body := []byte(`{"tools":[{"type":"function","function":{"name":"x"}}],"messages":[]}`)
	recordToolTraffic(body)
	recordToolTraffic(body)

	rows := toolUsageRows()
	if len(rows) != 1 {
		t.Fatalf("应只有 1 行，得到 %d", len(rows))
	}
	if rows[0].Declared != 2 {
		t.Errorf("声明次数应为 2，得到 %d", rows[0].Declared)
	}
	if !rows[0].Idle {
		t.Error("从没调用过应标记 idle")
	}
}
