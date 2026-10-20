// toolcalls.go 结构化工具调用的**统一判读**。
//
// 背景（一次跨层类型事故）：上游聚合产物里的 tool_calls 是 `[]map[string]any`
// （见 sse.go 的 Aggregate：calls := make([]map[string]any, 0, len(toolOrder))），
// 而 JSON 反序列化出来的 body 里是 `[]any`。两者的**动态类型不同**，
// 于是：
//
//   - 停摆检测的 hasToolCalls（只认 []any / map[string]any）认不出
//     Aggregate 产出的 `[]map[string]any` ⇒ 非流式路径把「工具其实调了」的
//     回合误判成停摆 ⇒ 白烧一次上游调用 + 白解绑一次粘性会话；
//   - DSML 修复器的守卫（`msg["tool_calls"].([]any)`）同样认不出
//     `[]map[string]any` ⇒ 修复器会用「正文标记还原出的调用」**覆盖真实的
//     tool_calls** ⇒ 调用参数被改写（实测 Hangzhou → Beijing）。
//
// 根因是同一件事在两层用了两套判读方式。本文件把判读收成**一个**函数，
// 两层都调它：
//
//   - HasStructuredToolCalls(v any) bool —— 判「这个值里有没有结构化工具调用」，
//     同时认 []any / []map[string]any / map[string]any / 单个调用对象。
//   - MessageHasToolCalls(msg map[string]any) bool —— 判「这条 message 有没有
//     工具调用」（含 function_call 老形态）。
//   - ToolCallList(v any) []map[string]any —— 取工具调用列表（空则 nil），
//     供需要逐个读调用参数的地方用。
//
// 刻意放在**独立文件**里：v1.13.0 基线分支（release/v1.13.0-fixed）不含
// dsml.go（上游 v1.13.0 之后的提交引入），所以本文件不能依赖 dsml.go 的任何
// 符号，两条分支才能共用同一份实现。
package upstream

// HasStructuredToolCalls 报告 v 里是否含有**结构化工具调用**。
//
// 认得全部已知的动态类型：
//   - []any               —— JSON 反序列化产物
//   - []map[string]any    —— Aggregate 产出的聚合形态
//   - map[string]any      —— 单个调用对象，或流式 delta 的 index→call 映射
//   - []ToolCall          —— 内部强类型（若存在）
//
// 空的集合（长度 0）不算「有调用」。
func HasStructuredToolCalls(v any) bool {
	return len(ToolCallList(v)) > 0
}

// ToolCallList 把 v 归一化成工具调用列表（空则返回 nil）。
//
// 对 map[string]any 有两种解释：单个调用对象（含 function 字段）或
// index→call 映射（流式 delta 的形态）。判定依据是「有没有 function 字段」：
// 有 → 单个调用对象；没有 → 视为映射，收集其中的调用对象。
func ToolCallList(v any) []map[string]any {
	switch t := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return t
	case map[string]any:
		// 单个调用对象？
		if _, ok := t["function"]; ok {
			return []map[string]any{t}
		}
		// index→call 映射（流式 delta 形态）。
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// MessageHasToolCalls 报告一条 message（OpenAI 形态）里是否含有工具调用。
//
// 同时认两种承载方式：
//   - tool_calls     —— OpenAI 现行形态
//   - function_call  —— 老形态（单对象）
func MessageHasToolCalls(msg map[string]any) bool {
	if msg == nil {
		return false
	}
	if HasStructuredToolCalls(msg["tool_calls"]) {
		return true
	}
	return HasStructuredToolCalls(msg["function_call"])
}
