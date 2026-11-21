package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// replayRoles 把回放出的消息角色串起来，便于断言批次有没有被拆开。
func replayRoles(out []any) string {
	var roles []string
	for _, m := range out {
		mm, _ := m.(map[string]any)
		roles = append(roles, asString(mm["role"]))
	}
	return strings.Join(roles, ",")
}

// TestReplayChatShapedToolKeepsPair 客户端按 Chat 形状回放的 tool 消息必须带着
// tool_call_id 出站：丢了它，cleanupOrphanToolCalls 看到的是「一条无结果的
// tool_call + 一条无主的 tool 消息」，会把这对调用与结果一起删掉 —— 上游读到的
// 历史里这轮工具调用凭空消失（reporter PR #272 第二处缺口的邻居）。
func TestReplayChatShapedToolKeepsPair(t *testing.T) {
	out := convertInputItems([]any{
		map[string]any{"role": "user", "content": "edit"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "ci", "type": "function",
				"function": map[string]any{"name": "noop", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "ci", "name": "noop", "content": "完成"},
		map[string]any{"role": "user", "content": "继续"},
	}, nil)

	var callID, resultID, resultName string
	for _, it := range out {
		m, _ := it.(map[string]any)
		switch asString(m["role"]) {
		case "assistant":
			if tcs, _ := m["tool_calls"].([]any); len(tcs) > 0 {
				callID = asString(tcs[0].(map[string]any)["id"])
			}
		case "tool":
			resultID = asString(m["tool_call_id"])
			resultName = asString(m["name"])
		}
	}
	if callID != "ci" || resultID != "ci" || resultName != "noop" {
		t.Fatalf("Chat 形状工具配对被拆：call=%q result=%q name=%q 出站=%v", callID, resultID, resultName, out)
	}
}

// TestReplayChatShapedToolCallIDAlias Responses 形状用 call_id，Chat 形状用
// tool_call_id；两个都认，否则同一份历史换个客户端就丢配对。
func TestReplayChatShapedToolCallIDAlias(t *testing.T) {
	out := convertInputItems([]any{
		map[string]any{"role": "tool", "call_id": "c9", "content": "结果"},
	}, nil)
	if len(out) != 1 {
		t.Fatalf("应只有一条消息：%v", out)
	}
	if got := asString(out[0].(map[string]any)["tool_call_id"]); got != "c9" {
		t.Fatalf("call_id 别名没被采纳：%q", got)
	}
}

// TestReplaySkipsHostedToolItems 托管工具与状态项（web_search_call /
// image_generation_call / mcp_call / compaction）不生成消息：此前落入默认分支变成
// 一条空的 user 消息，既拆开同一批工具调用（上游按 11148 判配对失败），又把图插进
// 并行 tool 结果之间，并把 image_generation_call 的 base64 成品送出站。
func TestReplaySkipsHostedToolItems(t *testing.T) {
	t.Run("夹在同一批工具调用之间", func(t *testing.T) {
		blob := strings.Repeat("iVBORw0KGgo", 4096)
		out := convertInputItems([]any{
			map[string]any{"role": "user", "content": "查完画图"},
			map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed",
				"action": map[string]any{"type": "search", "query": "q"}},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "noop", "arguments": "{}"},
			map[string]any{"type": "image_generation_call", "id": "ig_1", "status": "completed", "result": blob},
			map[string]any{"type": "function_call", "call_id": "c2", "name": "noop", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "一"},
			map[string]any{"type": "function_call_output", "call_id": "c2", "output": "二"},
		}, nil)
		if got := replayRoles(out); got != "user,assistant,tool,tool" {
			t.Fatalf("角色序列 %s：托管项造出了消息或拆开了批次（出站 %v）", got, out)
		}
		asst, _ := out[1].(map[string]any)
		if tcs, _ := asst["tool_calls"].([]any); len(tcs) != 2 {
			t.Fatalf("同一批工具调用被托管项拆成两批：%v", out)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "iVBORw0KGgo") {
			t.Fatal("image_generation_call 的 base64 成品出站了")
		}
	})

	t.Run("夹在并行工具结果之间", func(t *testing.T) {
		img := []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGk="}}
		out := convertInputItems([]any{
			map[string]any{"role": "user", "content": "看图"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "view", "arguments": "{}"},
			map[string]any{"type": "function_call", "call_id": "c2", "name": "view", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": img},
			map[string]any{"type": "mcp_call", "id": "mcp_1", "server_label": "s", "name": "t", "arguments": "{}", "output": "x"},
			map[string]any{"type": "function_call_output", "call_id": "c2", "output": "二"},
		}, nil)
		if got := replayRoles(out); got != "user,assistant,tool,tool,user" {
			t.Fatalf("角色序列 %s：图片被插进了并行 tool 结果之间（出站 %v）", got, out)
		}
	})

	t.Run("compaction 不造空消息", func(t *testing.T) {
		out := convertInputItems([]any{
			map[string]any{"role": "user", "content": "问"},
			map[string]any{"type": "message", "role": "assistant", "content": "答"},
			map[string]any{"type": "compaction", "encrypted_content": "前情提要 已改完"},
			map[string]any{"type": "message", "role": "assistant", "content": "再答"},
		}, nil)
		if got := replayRoles(out); got != "user,assistant,assistant" {
			t.Fatalf("compaction 造出了消息：%s（出站 %v）", got, out)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "前情提要") {
			t.Fatal("compaction 的密文被当内容送出去了")
		}
	})

	t.Run("带内容的未知项照旧送达", func(t *testing.T) {
		out := convertInputItems([]any{map[string]any{"type": "future_note", "content": "保留我"}}, nil)
		if len(out) != 1 || out[0].(map[string]any)["content"] != "保留我" {
			t.Fatalf("带内容的未知项被丢：%v", out)
		}
	})
}
