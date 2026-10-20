package upstream

import "testing"

// TestDeclaredToolNames 只认本请求自己声明的工具，不把会话历史里的工具算进来。
//
// 背景（issue #149 的误判风险）：停摆判定要问的是「**本轮**本该调工具却没调」。
// 如果把「历史里出现过的工具」也算成「本请求声明了工具」，那么多轮会话里
// 模型第 1 轮调了工具、第 2 轮给出最终答案（finish_reason=stop + 零 tool_calls）
// 这一轮会被误判成停摆 → 触发无谓的换号重试（多花一次上游调用 + 客户端多等一轮）。
func TestDeclaredToolNames(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string // 期望的工具名；nil = 期望不声明任何工具（不判停摆）
	}{
		{
			name: "标准 OpenAI tools 声明",
			body: `{"tools":[{"type":"function","function":{"name":"get_weather"}}]}`,
			want: []string{"get_weather"},
		},
		{
			name: "Responses 风格 tools（扁平 name）",
			body: `{"tools":[{"name":"get_weather"}]}`,
			want: []string{"get_weather"},
		},
		{
			name: "旧式 functions 声明",
			body: `{"functions":[{"name":"get_weather"}]}`,
			want: []string{"get_weather"},
		},
		{
			name: "多个工具",
			body: `{"tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"b"}}]}`,
			want: []string{"a", "b"},
		},
		{
			name: "历史里的 tool_calls **不算**本请求声明",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"get_weather"}}]}]}`,
			want: nil,
		},
		{
			name: "历史里的 function_call **不算**本请求声明",
			body: `{"messages":[{"role":"assistant","function_call":{"name":"get_weather"}}]}`,
			want: nil,
		},
		{
			name: "没有工具",
			body: `{"messages":[{"role":"user","content":"hi"}]}`,
			want: nil,
		},
		{
			name: "tool_choice=none 显式禁用 → 不判停摆",
			body: `{"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":"none"}`,
			want: nil,
		},
		{
			name: "tool_choice=None 大小写不敏感",
			body: `{"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":"None"}`,
			want: nil,
		},
		{
			name: "tool_choice 空格容错",
			body: `{"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":" none "}`,
			want: nil,
		},
		{
			name: "tool_choice 对象形式 none",
			body: `{"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":{"type":"none"}}`,
			want: nil,
		},
		{
			name: "tool_choice=auto 仍然算声明了工具",
			body: `{"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":"auto"}`,
			want: []string{"get_weather"},
		},
		{
			name: "tool_choice=required 仍然算声明了工具",
			body: `{"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":"required"}`,
			want: []string{"get_weather"},
		},
		{
			name: "请求体非法 → 空名单（不编造）",
			body: `{not json`,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeclaredToolNames([]byte(c.body))
			if len(c.want) == 0 {
				if len(got) != 0 {
					t.Errorf("want no declared tools, got %v", got)
				}
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			for _, w := range c.want {
				if !got[w] {
					t.Errorf("missing tool %q in %v", w, got)
				}
			}
		})
	}
}

// TestDeclaredToolNamesExcludesHistoryTools 停摆判定用的名单**只看本请求自己声明
// 的** tools，不含会话历史。
//
// （1.13.0 基线上没有标记修复层，所以没有「合并名单」可对比；这里直接钉住
// 「历史里的 tool_calls 不算本轮声明」这条语义。）
//
// 为什么必须排除历史：多轮会话里模型第 1 轮调了工具、第 2 轮给最终答案，
// 第 2 轮 finish_reason=stop + 零 tool_calls 是**完全正常**的收尾。若把历史
// 工具算进本轮声明，这个正常收尾会被误判成停摆 → 白白换号重试一次。
func TestDeclaredToolNamesExcludesHistoryTools(t *testing.T) {
	historyOnly := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"get_weather"}}]}]}`)
	if got := DeclaredToolNames(historyOnly); len(got) != 0 {
		t.Errorf("DeclaredToolNames must exclude history tools, got %v", got)
	}
	// 本请求自己声明的才算。
	declared := []byte(`{"tools":[{"type":"function","function":{"name":"get_weather"}}],"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"other_tool"}}]}]}`)
	got := DeclaredToolNames(declared)
	if !got["get_weather"] {
		t.Errorf("DeclaredToolNames must include own declared tools, got %v", got)
	}
	if got["other_tool"] {
		t.Errorf("DeclaredToolNames must exclude history tools, got %v", got)
	}
}
