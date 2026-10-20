// tool_pairing_dup_test.go 重复 tool_call id 的改名行为钉桩（issue #81）。
//
// 背景：某些客户端（Kimi 渠道）每条 assistant 消息都用「工具名:序号」形式
// 从 0 重新编号 tool_call id（SSHManager:0 / Bash:0 / MemoryManager:0 …），
// 一个会话里能攒出几十个同名 id。上游按「调用↔结果一一对应」校验，
// 多余的调用没有对应结果 → 400 code=11148 "tool calls and tool results do
// not match"，整条会话持续 503 no_healthy_account。
//
// 修法：第 2..n 次出现的同名 id 改名为 "<id>_d<n>"，并同步改写对应
// role:tool 结果的 tool_call_id，使配对重新一一对应。

package upstream

import "testing"

// TestCleanupOrphanToolCallsDuplicateIDs 重复 id 必须改名并保持配对。
func TestCleanupOrphanToolCallsDuplicateIDs(t *testing.T) {
	t.Run("重复 id 改名且结果同步改写", func(t *testing.T) {
		// 两次都用 "Bash:0"：第一次保持原样，第二次改名为 "Bash:0_d2"；
		// 两条 role:tool 结果按出现顺序消费改名槽位。
		in := msgs(t, `[
			{"role":"assistant","content":null,"tool_calls":[{"id":"Bash:0"}]},
			{"role":"tool","tool_call_id":"Bash:0","content":"r1"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"Bash:0"}]},
			{"role":"tool","tool_call_id":"Bash:0","content":"r2"}
		]`)
		out, changed := cleanupOrphanToolCalls(in)
		if !changed {
			t.Fatal("changed=false，重复 id 应触发改名")
		}
		assertSummary(t, summarize(out), []string{
			"assistant(Bash:0)",
			"tool(Bash:0)",
			"assistant(Bash:0_d2)",
			"tool(Bash:0_d2)",
		})
	})

	t.Run("三次同名 id：依次 _d2/_d3", func(t *testing.T) {
		in := msgs(t, `[
			{"role":"assistant","content":null,"tool_calls":[{"id":"X:0"}]},
			{"role":"tool","tool_call_id":"X:0","content":"r1"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"X:0"}]},
			{"role":"tool","tool_call_id":"X:0","content":"r2"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"X:0"}]},
			{"role":"tool","tool_call_id":"X:0","content":"r3"}
		]`)
		out, changed := cleanupOrphanToolCalls(in)
		if !changed {
			t.Fatal("changed=false，重复 id 应触发改名")
		}
		assertSummary(t, summarize(out), []string{
			"assistant(X:0)",
			"tool(X:0)",
			"assistant(X:0_d2)",
			"tool(X:0_d2)",
			"assistant(X:0_d3)",
			"tool(X:0_d3)",
		})
	})

	t.Run("改名后无孤儿：调用与结果仍然一一对应", func(t *testing.T) {
		in := msgs(t, `[
			{"role":"user","content":"go"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"Bash:0"}]},
			{"role":"tool","tool_call_id":"Bash:0","content":"r1"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"Bash:0"}]},
			{"role":"tool","tool_call_id":"Bash:0","content":"r2"}
		]`)
		out, _ := cleanupOrphanToolCalls(in)
		// 统计每个 id 出现在调用侧与结果侧的次数，必须相等。
		calls := map[string]int{}
		results := map[string]int{}
		for _, m := range out {
			msg, _ := m.(map[string]any)
			switch msg["role"] {
			case "assistant":
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tci := range tcs {
						tc, _ := tci.(map[string]any)
						id, _ := tc["id"].(string)
						calls[id]++
					}
				}
			case "tool":
				id, _ := msg["tool_call_id"].(string)
				results[id]++
			}
		}
		if len(calls) != len(results) {
			t.Fatalf("调用 id 集合与结果 id 集合不一致: calls=%v results=%v", calls, results)
		}
		for id, n := range calls {
			if results[id] != n {
				t.Errorf("id %q 调用 %d 次但结果 %d 次", id, n, results[id])
			}
		}
	})

	t.Run("id 不重复时零改动（不误伤正常会话）", func(t *testing.T) {
		in := msgs(t, `[
			{"role":"assistant","content":null,"tool_calls":[{"id":"c1"},{"id":"c2"}]},
			{"role":"tool","tool_call_id":"c1","content":"r1"},
			{"role":"tool","tool_call_id":"c2","content":"r2"}
		]`)
		if _, changed := cleanupOrphanToolCalls(in); changed {
			t.Error("changed=true，id 不重复不应改动")
		}
	})
}
