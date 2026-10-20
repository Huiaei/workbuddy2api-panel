// tool_stall.go 检测「agentic 请求声明了 tools，但上游一轮回来零 tool_calls
// 却以 stop 收尾」的停摆形态（issue #149）。
//
// 形态：请求里声明了 tools（客户端本意是要模型调工具），上游却只回了正文 +
// finish_reason=stop，没有任何 tool_calls。两种实测正文形态都属此类：
//   - 短正文（8~350 字符）：半句话预告，如「我这就去执行 X」；
//   - 长正文（400~1700 字符）：给一段说明/总结就收尾，任务并未推进。
//
// 两种形态都会让客户端按「正常完成」收尾 → 工具一次没跑、任务停在原地，用户
// 必须手动催一句才继续。已排除的假设（报告方实测）：不是客户端丢弃、不是网关
// 丢弃（原始 SSE 里就没有 tool_calls 帧）、不是思考内容导致、不是历史回传导致、
// 不是单一模型问题（cn 域四个模型都有，深seek 系比例最高）。
//
// 判定口径：**只按「零 tool_calls」判，绝不按正文长度判**。报告方自己的教训是
// 按长度阈值判会把 glm-5.3 的停摆率算成 0%（它的 3 次停摆正文都是 1000+ 字符），
// 实际按零 tool_calls 算是 13%。
//
// 重要前提：这是**上游行为缺陷**，不是账号问题。所以命中后**绝不罚账号**
// （不冷却、不熔断、不 NoteFailures）——与 ErrChannelRefused 同一处置原则。
package upstream

import (
	"encoding/json"
	"strings"
)

// DeclaredToolNames 收集**本请求自己声明**的工具名集合（不含会话历史）。
//
// 返回 nil 表示「本请求不该有工具调用」，调用方据此**不判停摆**。三种情况
// 返回 nil：
//  1. 请求体解析失败（不编造名单，宁可漏判不可误判）；
//  2. 本请求没声明任何 tools/functions；
//  3. 本请求用 tool_choice="none" **显式禁止**了工具调用——此时模型只回正文
//     并以 stop 收尾是**完全正确**的行为，把它当停摆是纯误判。
//
// 与 ToolNameAllowlist 的区别（这正是停摆判定必须用本函数的原因）：
// ToolNameAllowlist 把「本请求声明的工具」和「会话历史里出现过的工具」合并。
// 合并名单适合标记修复（判定「这段正文里的工具标记是不是我们认识的工具」），
// 但**不适合**停摆判定——
//
// 历史里有 tool_calls ≠ 本轮该调工具。多轮会话里，模型在第 1 轮调了工具、
// 第 2 轮给出最终答案，这第 2 轮是**完全正常**的收尾（finish_reason=stop +
// 零 tool_calls）。若用合并名单判定，第 2 轮会被误判成「停摆」→ 触发无谓的
// 换号重试（白花一次上游调用 + 让客户端多等一轮），把一个正常回答变成两次请求。
//
// 停摆的语义是「**本轮本该调工具却没调**」——只有本请求自己声明了 tools
// 且没有禁止工具调用，才有「本该调」这回事。
func DeclaredToolNames(body []byte) map[string]bool {
	var req struct {
		Tools []struct {
			Name     string `json:"name"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Functions []struct {
			Name string `json:"name"`
		} `json:"functions"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	// tool_choice="none"（或 {"type":"none"}）= 显式禁用工具调用 → 不判停摆。
	if toolChoiceNone(req.ToolChoice) {
		return nil
	}
	names := map[string]bool{}
	for _, t := range req.Tools {
		if t.Function != nil && t.Function.Name != "" {
			names[t.Function.Name] = true
			continue
		}
		if t.Name != "" {
			names[t.Name] = true
		}
	}
	for _, f := range req.Functions {
		if f.Name != "" {
			names[f.Name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// HistoryHasToolResult 判断**会话历史**里是否已经出现过「工具结果」。
//
// 为什么停摆判定需要它：真实 agentic 客户端（DSH、Codex、Cline…）**每一轮都会
// 重发同一份 tools 声明**。于是多轮会话的**正常收尾轮**（模型给出最终答案、
// finish_reason=stop、零 tool_calls）也满足「本请求声明了 tools」的表面条件——
// 若只看这一条，每一轮正常收尾都会被误判成停摆，触发一次无谓的换号重试
// （多花一次上游调用 + 一次计费），把一个正常回答变成两次请求。
//
// 判据取「历史里出现过 role=tool 的结果消息」而不是「出现过 tool_calls」：
//   - 历史里有 tool_calls 但**还没有**工具结果 → 说明上一轮刚要调工具，本轮
//     拿到的正文就停住了 → **正是**停摆的典型现场，必须判停摆；
//   - 历史里已经有工具结果 → 说明工具**跑过了**，本轮的正文是最终答案 → 不是停摆。
//
// 解析失败一律返回 false（= 允许判停摆）：宁可多判一次（多一次重试），
// 也不要把真停摆放过（放过 = 工具永远不跑，任务卡死）。
func HistoryHasToolResult(body []byte) bool {
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "tool" || m.Role == "function" {
			return true
		}
	}
	return false
}

// ToolChoiceForcesTool 判断 tool_choice 是否**点名要求**调某个具体函数。
//
// 形态：{"type":"function","function":{"name":"get_weather"}}（OpenAI 规范）。
//
// 意义：客户端点名要调哪个函数时，「零 tool_calls」是**确定**的停摆——模型被明确
// 要求调这个函数却只回了正文，工具一定没跑。这条比「声明了 tools」精确得多，
// 不受「模型选择直接回答」这类合理行为干扰。
//
// 返回 true 时，停摆判定不再需要「历史里没有工具结果」这一条辅助条件。
func ToolChoiceForcesTool(body []byte) bool {
	var req struct {
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	raw := req.ToolChoice
	if len(raw) == 0 {
		return false
	}
	var obj struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(obj.Type), "function") {
		return false
	}
	return obj.Function != nil && strings.TrimSpace(obj.Function.Name) != ""
}

// toolChoiceNone 判断 tool_choice 是否显式禁用了工具调用。
//
// 形态：字符串 "none"（大小写不敏感、可带空白）或对象 {"type":"none"}。
// 与 internal/upstream/payload.go 的 normalizeToolChoice 保持同一口径——
// 那里在 "none" 时会把 tools/functions 从出站 body 里删掉（上游根本收不到工具
// 声明），所以停摆判定也必须把这一路排除，否则会对一次刻意禁用工具的请求
// 报「本该调工具却没调」。
func toolChoiceNone(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.EqualFold(strings.TrimSpace(s), "none")
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		typ, _ := obj["type"].(string)
		return strings.EqualFold(strings.TrimSpace(typ), "none")
	}
	return false
}

// ToolStallInfo 描述一次「零 tool_calls 停摆」的判定结果。
type ToolStallInfo struct {
	// Suspect 为 true 表示本轮疑似停摆（声明了 tools 却零 tool_calls 且 stop 收尾）。
	Suspect bool
	// FinishReason 本轮的收尾原因（通常为 "stop"）。
	FinishReason string
	// ContentLen 本轮正文长度（仅供日志观测，**不参与判定**）。
	ContentLen int
	// HasText 本轮是否有正文（空正文 + 零 tool_calls 同样是停摆）。
	HasText bool
	// DeclaredTools 本次请求声明的工具名数量（观测用）。
	DeclaredTools int
}

// DetectToolStall 判断一个**聚合后的**非流式响应是否是「零 tool_calls 停摆」。
//
// declaredTools 是**本请求自己声明**的工具名集合（见 DeclaredToolNames）——为空
// 表示本请求没有声明任何工具，此时不存在「本该调工具却没调」的问题，一律不判停摆。
//
// historyHasToolResult 表示**会话历史里已经出现过工具结果**（见
// HistoryHasToolResult）。为 true 时**不判停摆**：工具已经跑过了，本轮的正文
// 就是最终答案——真实 agentic 客户端每轮都重发同一份 tools 声明，不加这一条就会
// 把每一次正常收尾都误判成停摆。
//
// resp 是 upstream.Aggregate 的输出（choices[].message + choices[].finish_reason）。
//
// 判定条件（四条全满足才算停摆）：
//  1. 本请求声明了 tools（declaredTools 非空）；
//  2. 会话历史里还没有工具结果（historyHasToolResult=false）；
//  3. 本轮响应**没有任何** tool_calls（message.tool_calls 为空或缺失）；
//  4. 收尾原因是 "stop"（其他收尾原因如 length / tool_calls / content_filter
//     各自有别的语义，不算停摆）。
//
// 第 4 条是关键的收敛条件：「模型只说了话就收尾」在**没有** tools 声明的普通
// 对话里是完全正常的行为；有 tools 声明时它才是异常信号。
func DetectToolStall(resp map[string]any, declaredTools map[string]bool, historyHasToolResult bool, forcedTool bool) ToolStallInfo {
	info := ToolStallInfo{DeclaredTools: len(declaredTools)}
	if len(declaredTools) == 0 || resp == nil {
		return info
	}
	if historyHasToolResult && !forcedTool {
		// 历史里已经有工具结果 → 工具跑过了，本轮是最终答案 → 不是停摆。
		//
		// 例外：客户端用 tool_choice 点名了要调某个函数 → 「零 tool_calls」是确定
		// 的停摆（模型被明确要求调它却只回正文），此时历史判定不适用。
		return info
	}
	// 截断排除：上游流被截断（没发 [DONE] 就 EOF）时，本轮的零 tool_calls 可能是
	// 「工具调用帧还没到就被切断」，不是「模型决定不调工具」。传输层截断是另一
	// 类问题，不能记成上游行为缺陷（issue #149 的停摆率统计会被污染）。
	if trunc, _ := resp["_truncated"].(bool); trunc {
		return info
	}
	choices, ok := resp["choices"].([]any)
	if !ok {
		return info
	}
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		fr, _ := c["finish_reason"].(string)
		msg, _ := c["message"].(map[string]any)
		if msg == nil {
			// 没有 message 段（异常形状）→ 不判停摆（宁可漏判不可误判）。
			continue
		}
		if hasToolCalls(msg) {
			// 本轮有工具调用 → 正常，不算停摆。
			info.FinishReason = fr
			return info
		}
		if fr != "stop" {
			// 非 stop 收尾：length（截断）/ tool_calls（走了工具但被上游换形态）
			// / content_filter 等，各有别的语义，不算停摆。
			info.FinishReason = fr
			return info
		}
		// 到这里：本轮零 tool_calls 且以 stop 收尾。
		info.FinishReason = fr
		if txt, ok := msg["content"].(string); ok {
			info.ContentLen = len(txt)
			info.HasText = strings.TrimSpace(txt) != ""
			// 自防御：正文里含**模型原生工具调用标记** ⇒ 上游其实调了工具，
			// 只是把调用吐成了文本标记（见 dsml.go 的背景说明）。
			// 这种形态**绝不能**判停摆，否则会：
			//   - 白白换号重试一次（多花一次上游调用、客户端多等一轮）；
			//   - 在没有标记修复层的分支上把「工具其实调了」的回合误报成停摆，
			//     污染停摆率统计（issue #149 的可观测性会失真）。
			//
			// 正常路径上标记已在修复层被还原成结构化 tool_calls（此时
			// hasToolCalls 已在上方返回 true），这条自防御是**兜底**：
			// 任何未来新增的调用方忘了先跑修复层，也不会把误判带进来。
			if containsMarkupCall(txt) {
				info.FinishReason = fr
				return info
			}
		}
		info.Suspect = true
		return info
	}
	return info
}

// markupCallOpenTag 是模型原生工具调用标记的**开始标记**。
//
// 与 dsml.go 的 markupOpen 同值（main 分支里 markupOpen 直接由本常量派生，
// 保证「修复层认的标记」与「停摆检测认的标记」永远一致，不会各自漂移）。
//
// 这里刻意**不引用** dsml.go 的常量：v1.13.0 基线分支（release/v1.13.0-fixed）
// 不含 dsml.go（那是上游 v1.13.0 之后的 9 个提交之一引入的），引用它会让
// 基线分支编译不过。
const markupCallOpenTag = "<\uFF5C\uFF5C" + "DSML" + "\uFF5C\uFF5C calls>"

// containsMarkupCall 判断文本里是否含模型原生工具调用标记的**开始标记**。
//
// 用开始标记（markupCallOpenTag）而不是完整的块：开始标记已经足以说明「上游在
// 正文里写了工具调用」，而完整块可能因分片边界不完整（增量到达时块被切成两片）。
//
// 标记块被修复层处理过时，会从正文里被**移走**（dsml.go 的 Feed 会把块内容
// 替换成空串），所以修复后这里读不到标记 —— 这正是我们要的：
// 「修复过 → 有结构化 tool_calls → 不是停摆」与「没修复 → 正文含标记 → 也不是
// 停摆」两条路径口径一致。
func containsMarkupCall(text string) bool {
	if text == "" {
		return false
	}
	return strings.Contains(text, markupCallOpenTag)
}

// hasToolCalls 判断聚合响应的 message 段里是否带了 tool_calls。
//
// 兼容三种形态：
//   - OpenAI 标准：message.tool_calls 是数组；
//   - 上游偶发用对象：message.tool_calls 是单个对象；
//   - Responses 风格：message.function_call 是对象。
func hasToolCalls(msg map[string]any) bool {
	// 统一走 MessageHasToolCalls：它同时认 `[]any`（JSON 反序列化产物）与
	// `[]map[string]any`（Aggregate 产出的聚合形态）。早先这里只认 []any，
	// 而 Aggregate 产出的是 []map[string]any ⇒ 非流式路径把「工具其实调了」
	// 的回合误判成停摆（白烧一次上游调用 + 白解绑一次粘性会话）。
	return MessageHasToolCalls(msg)
}

// StreamToolStallDetector 在**流式**路径上做同样的判定。
//
// 用途：流式响应一旦把帧写给客户端就收不回来了，无法在「整轮结束后」再决定是否
// 重试。但它仍然有两个价值：
//  1. **观测**：给出「本轮疑似停摆」的结论，写进请求日志与 stdout，让运维一眼
//     看到停摆率与模型分布（而不是只能靠客户端侧统计）。
//  2. **兜底判定**：流式与非流式的停摆定义一致，同一套观测口径。
//
// 它不改变任何透传字节（纯旁路），也不重试（重试决策放在非流式路径与客户端侧）。
type StreamToolStallDetector struct {
	// declaredTools 本请求声明的工具名集合。
	declaredTools map[string]bool
	// toolCallSeen 记录是否见过任何 tool_calls delta。
	toolCallSeen bool
	// finishReason 记录见到的收尾原因。
	finishReason string
	// contentLen 累计正文长度。
	contentLen int
	// truncated 记录上游流是否被截断（没发 [DONE] 就 EOF）。截断时的零
	// tool_calls 是「帧没到齐」，不是停摆。
	truncated bool
	// repaired 记录本流是否由标记修复器还原出过工具调用（上游吐的是标记文本
	// 而不是结构化 tool_calls）。还原过 = 上游**确实调了工具** → 不是停摆。
	// 没有这个信号时流式与非流式会给出相反结论（非流式先修复再判定 → 不停摆；
	// 流式观测原始帧 → 停摆），同一形态两种口径，统计不可用。
	repaired bool
	// historyHasToolResult 会话历史里已经出现过工具结果 → 不判停摆
	// （与 DetectToolStall 同口径：工具跑过了，本轮是最终答案）。
	historyHasToolResult bool
	// forcedTool 客户端用 tool_choice 点名了要调某个函数 → 「零 tool_calls」是
	// 确定的停摆（模型被明确要求调它却只回正文），历史判定不适用。
	forcedTool bool
}

// NewStreamToolStallDetector 构造流式停摆探测器。
//
// historyHasToolResult 表示会话历史里已经出现过工具结果；为 true 时**不判停摆**
// （与 DetectToolStall 同口径）。
func NewStreamToolStallDetector(declaredTools map[string]bool, historyHasToolResult bool, forcedTool bool) *StreamToolStallDetector {
	return &StreamToolStallDetector{declaredTools: declaredTools, historyHasToolResult: historyHasToolResult, forcedTool: forcedTool}
}

// MarkTruncated 标记本流被截断（上游没发 [DONE] 就 EOF 收尾）。
// 截断的零 tool_calls 不算停摆。
//
// 签名与 WithStreamEndObserver 的回调一致（func(truncated bool)）：回调收到
// truncated=true 时才置位，truncated=false 什么都不做。
func (d *StreamToolStallDetector) MarkTruncated(truncated bool) {
	if d != nil && truncated {
		d.truncated = true
	}
}

// MarkRepaired 标记本流由标记修复器还原出过工具调用（上游吐的是原生标记文本）。
// 还原过 → 上游确实调了工具 → 不算停摆（与非流式口径对齐）。
func (d *StreamToolStallDetector) MarkRepaired(n int) {
	if d != nil && n > 0 {
		d.repaired = true
	}
}

// Observe 观察一个上游帧（透传前的原始 chunk 对象）。只读不改。
//
// 帧形态：{"choices":[{"delta":{"tool_calls":[...]},"finish_reason":"stop"}]}
func (d *StreamToolStallDetector) Observe(obj map[string]any) {
	if d == nil || obj == nil {
		return
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return
	}
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			d.finishReason = fr
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		// 判读认全类型（[]any / []map[string]any / 单个调用对象）：流式 delta
		// 里的 tool_calls 形态随上游而异（整包 delta、index→call 映射、
		// 分片增量都可能），只认 []any 会漏判「工具其实调了」。
		if HasStructuredToolCalls(delta["tool_calls"]) {
			d.toolCallSeen = true
		}
		if HasStructuredToolCalls(delta["function_call"]) {
			d.toolCallSeen = true
		}
		if txt, ok := delta["content"].(string); ok {
			d.contentLen += len(txt)
		}
	}
}

// Info 返回当前的停摆判定结果。
func (d *StreamToolStallDetector) Info() ToolStallInfo {
	if d == nil {
		return ToolStallInfo{}
	}
	info := ToolStallInfo{
		FinishReason:  d.finishReason,
		ContentLen:    d.contentLen,
		HasText:       d.contentLen > 0,
		DeclaredTools: len(d.declaredTools),
	}
	if len(d.declaredTools) == 0 || d.toolCallSeen {
		return info
	}
	if d.historyHasToolResult && !d.forcedTool {
		// 历史里已经有工具结果 → 工具跑过了，本轮是最终答案 → 不是停摆。
		return info
	}
	if d.truncated {
		// 截断：工具调用帧可能还没到就被切断，不是「模型决定不调工具」。
		return info
	}
	if d.repaired {
		// 标记修复器还原出过调用 → 上游确实调了工具（只是吐成了文本标记）。
		return info
	}
	if d.finishReason != "stop" {
		return info
	}
	info.Suspect = true
	return info
}

// MarshalJSON 让 ToolStallInfo 能直接进请求日志（字段名与其它观测字段一致风格）。
func (t ToolStallInfo) MarshalJSON() ([]byte, error) {
	type alias ToolStallInfo
	return json.Marshal(alias(t))
}
