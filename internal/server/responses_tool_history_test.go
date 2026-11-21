package server

import (
	"encoding/json"
	"testing"
)

// Codex 会把同一轮的助手正文与工具调用拆成两个 input item（phase=commentary 的
// message 项 + 紧邻的 function_call 项）。回放成两条 assistant 轮时，部分模型
// （kimi/glm 系）只回一句进度就收尾，不再续做工具调用（reporter PR #281 实测）。
//
// 变异验证：把 flush() 里的 toolTurnText 分支撤掉（总是新建 assistant 消息）
// ⇒ 本用例失败（消息数 4、assistant 两条、第一条没有 tool_calls）。
func TestResponsesMergesAssistantTextWithToolCalls(t *testing.T) {
	body, _, _, err := responsesToChat([]byte(`{"model":"kimi-k3.1-pro","input":[
		{"role":"user","content":"fix tests"},
		{"role":"assistant","content":"running tests"},
		{"type":"function_call","call_id":"c1","name":"check","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"check","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"failed"},
		{"type":"function_call_output","call_id":"c2","output":"ok"},
		{"type":"function_call","call_id":"c3","name":"check","arguments":"{}"},
		{"type":"function_call_output","call_id":"c3","output":"fixed"},
		{"role":"assistant","content":"done"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	var chat struct{ Messages []map[string]any }
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatal(err)
	}
	msgs := chat.Messages
	// user / assistant(正文+两个调用) / tool c1 / tool c2 / assistant(一个调用) / tool c3 / assistant 收尾
	if len(msgs) != 7 {
		t.Fatalf("想要 7 条消息（同轮正文与调用合并），实际 %d：%v", len(msgs), msgs)
	}
	if role, _ := msgs[0]["role"].(string); role != "user" {
		t.Fatalf("第一条应是 user：%v", msgs[0])
	}
	m := msgs[1]
	if role, _ := m["role"].(string); role != "assistant" {
		t.Fatalf("第二条应是 assistant：%v", m)
	}
	if c, _ := m["content"].(string); c != "running tests" {
		t.Fatalf("同轮正文必须保留在带 tool_calls 的那条消息上，实际 content=%v", m["content"])
	}
	calls, _ := m["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("同轮并行调用应挂在这条消息上（2 个），实际 %d：%v", len(calls), m)
	}
	for i, id := range []string{"c1", "c2"} {
		call, _ := calls[i].(map[string]any)
		if got, _ := call["id"].(string); got != id {
			t.Fatalf("第 %d 个调用 id = %v，想要 %s", i, call["id"], id)
		}
		if got, _ := msgs[i+2]["tool_call_id"].(string); got != id {
			t.Fatalf("工具结果 %d 配对错位：%v", i, msgs[i+2])
		}
	}
	// 第二轮（c3）是新的 assistant 消息，不得并进第一条。
	second := msgs[4]
	if role, _ := second["role"].(string); role != "assistant" {
		t.Fatalf("第二轮应是 assistant：%v", second)
	}
	if c, _ := second["content"].(string); c != "" {
		t.Fatalf("新建的 assistant 轮 content 应为空，实际 %v", second["content"])
	}
	c3, _ := second["tool_calls"].([]any)
	if len(c3) != 1 {
		t.Fatalf("第二轮应有 1 个调用，实际 %v", second["tool_calls"])
	}
	if got, _ := msgs[6]["content"].(string); got != "done" {
		t.Fatalf("收尾正文应为 done，实际 %v", msgs[6]["content"])
	}
}

// 跨轮不得合并：user 消息、工具结果都是边界。
func TestResponsesToolHistoryDoesNotMergeAcrossTurnBoundary(t *testing.T) {
	var items []any
	input := `[
		{"role":"assistant","content":"done"},
		{"role":"user","content":"next"},
		{"type":"function_call","call_id":"c9","name":"check","arguments":"{}"}
	]`
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		t.Fatal(err)
	}
	out := convertInputItems(items, nil)
	if len(out) != 3 {
		t.Fatalf("想要 3 条消息（assistant 收尾 / user / 新建 assistant+调用），实际 %d：%v", len(out), out)
	}
	first, _ := out[0].(map[string]any)
	if first["tool_calls"] != nil {
		t.Fatalf("工具调用被错误并进了上一条 assistant 消息：%v", out)
	}
	last, _ := out[2].(map[string]any)
	if role, _ := last["role"].(string); role != "assistant" {
		t.Fatalf("跨 user 边界后应新建 assistant 轮：%v", out)
	}
	if calls, _ := last["tool_calls"].([]any); len(calls) != 1 {
		t.Fatalf("新的 assistant 轮应带 1 个调用：%v", out)
	}
}
