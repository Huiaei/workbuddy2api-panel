package upstream

import (
	"encoding/json"
	"strings"
)

// maxSplitParts 上游粘帧拆分时允许切出的最大段数上限。
//
// 背景：上游偶发把两个 JSON chunk 塞进同一行 data（"data: {a} {b}"）。客户端按
// SSE 规范把整行当一份 JSON 解析，会报 "Unexpected non-whitespace character
// after JSON at position N"，整轮作废。网关在透传前把粘连的多段 JSON 拆成独立
// 帧即可修复。
//
// 上限是为了防病态输入（一行里塞几百个 "{}"）把单帧膨胀成几百帧、白烧 CPU 与
// 带宽。达到上限即放弃拆分（保持原样透传）：宁可这一帧解析失败，也不能让网关
// 被一行输入放大成几百次下游写出。
const maxSplitParts = 64

// looksLikeJSON 判断 payload 看起来像 JSON（以 { 或 [ 开头，忽略前导空白）。
//
// 用途是决定「解析失败」时要不要告警：上游真发了 JSON 却解析不出来是异常形态
// （粘帧、被截断、字段损坏），值得 WARN；少数上游会在 data 里塞纯文本，那属于
// 正常降级透传，不该刷告警。
func looksLikeJSON(payload string) bool {
	t := strings.TrimLeft(payload, " \t\r\n")
	if t == "" {
		return false
	}
	return t[0] == '{' || t[0] == '['
}

// splitConcatenatedJSON 把粘在一行的多段 JSON 拆成独立片段（每段都是完整 JSON）。
//
// 扫描规则：按字节推进，跟踪「是否在字符串内」「转义是否待处理」以及 {} / [] 的
// 嵌套深度；一段 JSON 值读完（复合值回到深度 0、字符串闭合、字面量读到分隔符）
// 就切一段。
//
// 保守原则（宁可少切，也不要产出半截 JSON）：
//   - 每一段都必须 json.Valid 通过，否则整行放弃拆分、原样返回；
//   - 遇到残缺值（未闭合的括号/字符串）或不认识的起始字符，整行放弃拆分；
//   - 段数达到 maxSplitParts 时放弃拆分。
//
// 放弃拆分时返回的 slice 长度为 1、内容就是原始 payload —— 调用方据此走原来的
// 「原样透传、不计数」路径。
func splitConcatenatedJSON(payload string) []string {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return []string{payload}
	}
	var parts []string
	i := 0
	for i < len(trimmed) {
		// 跳过段与段之间的空白。
		for i < len(trimmed) && isJSONSpace(trimmed[i]) {
			i++
		}
		if i >= len(trimmed) {
			break
		}
		// 段数上限：再多一段就超过上限 → 放弃拆分（不产出截断结果）。
		if len(parts) >= maxSplitParts {
			return []string{payload}
		}
		end, ok := jsonValueEnd(trimmed, i)
		if !ok {
			// 切不出完整的一段（残缺 JSON / 未知起始字符）⇒ 放弃拆分。
			return []string{payload}
		}
		seg := trimmed[i:end]
		if !json.Valid([]byte(seg)) {
			// 半截/非法 JSON 绝不放行。
			return []string{payload}
		}
		parts = append(parts, seg)
		i = end
	}
	if len(parts) == 0 {
		return []string{payload}
	}
	return parts
}

// isJSONSpace 判断字节是不是 JSON 语法里的空白。
func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// jsonValueEnd 返回 s[i] 处开始的**一个**完整 JSON 值的结束下标（不含）。
// ok=false 表示这段切不完整（未闭合 / 起始字符不认识）。
func jsonValueEnd(s string, i int) (int, bool) {
	if i >= len(s) {
		return 0, false
	}
	switch c := s[i]; c {
	case '{', '[':
		return compositeValueEnd(s, i)
	case '"':
		return stringValueEnd(s, i)
	default:
		// 数字 / true / false / null：扫到下一个空白或结构性字符为止。
		j := i
		for j < len(s) && !isJSONSpace(s[j]) && s[j] != '{' && s[j] != '}' &&
			s[j] != '[' && s[j] != ']' && s[j] != ',' && s[j] != ':' && s[j] != '"' {
			j++
		}
		if j == i {
			return 0, false
		}
		return j, true
	}
}

// compositeValueEnd 扫描以 { 或 [ 起始的复合值：跟踪 {} / [] 嵌套深度，遇到字符串
// 内部的括号/引号不计入深度（转义引号也不结束字符串）。深度回到 0 的位置即该值
// 的末尾。
func compositeValueEnd(s string, i int) (int, bool) {
	depth := 0
	inStr := false
	escaped := false
	for j := i; j < len(s); j++ {
		c := s[j]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return j + 1, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	// 走到结尾仍未闭合 ⇒ 残缺值。
	return 0, false
}

// stringValueEnd 扫描一个 JSON 字符串值（从起始引号到收尾引号），处理转义。
func stringValueEnd(s string, i int) (int, bool) {
	escaped := false
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}
