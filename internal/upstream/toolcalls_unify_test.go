package upstream

import "testing"

// 本文件钉住**交叉审计第二轮** D3/D4 抓到的那类缺陷：
//
// 同一个事实在**两层**用了两套判读方式，而两层的动态类型不同：
//   - Aggregate 产出的 message.tool_calls 是 `[]map[string]any`；
//   - JSON 反序列化出来的 body 里是 `[]any`。
//
// 后果：
//   - 停摆检测的 hasToolCalls 只认 []any ⇒ 非流式路径把「工具其实调了」的
//     回合误判成停摆（D3）⇒ 白烧一次上游调用 + 白解绑一次粘性会话；
//   - DSML 修复器的守卫同样认不出 ⇒ 修复器用「正文标记还原出的调用」覆盖
//     真实 tool_calls（D4）⇒ 调用参数被改写（实测 Hangzhou → Beijing）。
//
// 修法：把判读收成**一个**函数（toolcalls.go 的 HasStructuredToolCalls /
// MessageHasToolCalls / ToolCallList），两层都调它。

// TestHasStructuredToolCallsRecognizesAllShapes 判读函数必须认全已知形态。
func TestHasStructuredToolCallsRecognizesAllShapes(t *testing.T) {
	call := map[string]any{"id": "c1", "function": map[string]any{"name": "get_weather"}}

	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"JSON 形态 []any", []any{call}, true},
		{"聚合形态 []map[string]any", []map[string]any{call}, true},
		{"单个调用对象", call, true},
		{"流式 index→call 映射", map[string]any{"0": call}, true},
		{"空 []any", []any{}, false},
		{"空 []map[string]any", []map[string]any{}, false},
		{"空 map", map[string]any{}, false},
		{"nil", nil, false},
		{"非工具值", "text", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasStructuredToolCalls(tc.v); got != tc.want {
				t.Fatalf("HasStructuredToolCalls(%#v)=%v want %v", tc.v, got, tc.want)
			}
		})
	}
}

// TestMessageHasToolCallsRecognizesAggregateShape D3 的直接回归：
// 一条 message 的 tool_calls 是聚合形态（[]map[string]any）时，必须判为
// 「本轮有工具调用」。
func TestMessageHasToolCallsRecognizesAggregateShape(t *testing.T) {
	call := map[string]any{"id": "c1", "function": map[string]any{"name": "get_weather"}}

	if !MessageHasToolCalls(map[string]any{"tool_calls": []map[string]any{call}}) {
		t.Fatal("聚合形态的 tool_calls 必须被认出（D3）")
	}
	if !MessageHasToolCalls(map[string]any{"tool_calls": []any{call}}) {
		t.Fatal("JSON 形态的 tool_calls 必须被认出")
	}
	if !MessageHasToolCalls(map[string]any{"function_call": call}) {
		t.Fatal("老形态 function_call 必须被认出")
	}
	if MessageHasToolCalls(map[string]any{"content": "hi"}) {
		t.Fatal("无工具调用的 message 不得判为有")
	}
	if MessageHasToolCalls(nil) {
		t.Fatal("nil message 不得判为有")
	}
}

// TestHasToolCallsSeesAggregateShape hasToolCalls（停摆检测的判读）必须与
// MessageHasToolCalls 同口径 —— 否则非流式路径会把「工具其实调了」的回合
// 误判成停摆。
func TestHasToolCallsSeesAggregateShape(t *testing.T) {
	call := map[string]any{"id": "c1", "function": map[string]any{"name": "get_weather"}}
	if !hasToolCalls(map[string]any{"tool_calls": []map[string]any{call}}) {
		t.Fatal("hasToolCalls 必须认出聚合形态（D3）")
	}
	if hasToolCalls(map[string]any{"tool_calls": []map[string]any{}}) {
		t.Fatal("空聚合不得判为有调用")
	}
}

// TestToolCallListNormalizesShapes ToolCallList 要把各形态归一化成同一份列表。
func TestToolCallListNormalizesShapes(t *testing.T) {
	call := map[string]any{"id": "c1"}
	if got := ToolCallList([]any{call}); len(got) != 1 || got[0]["id"] != "c1" {
		t.Fatalf("[]any 归一化失败: %+v", got)
	}
	if got := ToolCallList([]map[string]any{call}); len(got) != 1 || got[0]["id"] != "c1" {
		t.Fatalf("[]map[string]any 归一化失败: %+v", got)
	}
	if got := ToolCallList(map[string]any{"function": map[string]any{"name": "f"}}); len(got) != 1 {
		t.Fatalf("单个调用对象归一化失败: %+v", got)
	}
	if got := ToolCallList(map[string]any{"0": call}); len(got) != 1 {
		t.Fatalf("index→call 映射归一化失败: %+v", got)
	}
	if got := ToolCallList(nil); got != nil {
		t.Fatalf("nil 应归一化为 nil: %+v", got)
	}
}

// TestBusinessCodeAliasesRecognized 业务码的**键名别名**必须被认出。
//
// 上游现网用 "code"，但同一套错误信封在不同端点/版本下出现过别名
// （errCode / errorCode / error_code / bizCode）。只认字面键名 "code" 的话，
// 一旦上游换键名，IsChannelRefused 与 IsAccountRestricted 会**同时失明**：
// 11-128 与 11140 双双落通用 4xx ⇒ 好号被降权出池（连败计数），真封禁也
// 检不出（永不禁用）。
func TestBusinessCodeAliasesRecognized(t *testing.T) {
	aliases := []string{"code", "errCode", "errorCode", "error_code", "bizCode", "biz_code"}
	for _, key := range aliases {
		body := `{"` + key + `":11140,"msg":"request illegal"}`
		if !hasBusinessCode(body, "11140") {
			t.Errorf("业务码键名 %q 必须被认出", key)
		}
		if !IsAccountRestricted(body) {
			t.Errorf("业务码键名 %q 下 11140 必须判为账号封禁", key)
		}
		ch := `{"` + key + `":11128,"msg":"request illegal"}`
		if !IsChannelRefused(ch) {
			t.Errorf("业务码键名 %q 下 11128 必须判为通道校验", key)
		}
	}
	// 嵌套位置同样要认（上游信封常把码放在 error / data 下）。
	if !hasBusinessCode(`{"error":{"data":{"errCode":11140}}}`, "11140") {
		t.Error("嵌套位置的 errCode 必须被认出")
	}
	// BusinessCodeOf 供错误文案标注实际业务码。
	if got := BusinessCodeOf(`{"error":{"code":11140}}`); got != "11140" {
		t.Errorf("BusinessCodeOf=%q want 11140", got)
	}
	if got := BusinessCodeOf(`{"msg":"x"}`); got != "" {
		t.Errorf("无业务码时 BusinessCodeOf 应为空，got %q", got)
	}
}
