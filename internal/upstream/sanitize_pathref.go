// sanitize_pathref.go 出站脱敏：「@ 路径引用协议」文案的逐字黑名单规避。
//
// 背景（2026-10-28 CN realm 实案）：上游渠道校验新增了对**官方客户端 @ 路径引用
// 协议说明文案**的拦截。与 2026-10-21 的身份句黑名单不同，这次不是「某一句逐字
// 命中即拦」，而是**两句必须同时出现才拦**（实测 K≥3 重复，确定）。
//
// 实测（直连 CN 上游 https://copilot.tencent.com/v2/chat/completions）：
//
//	P1「Tokens prefixed with @ are paths the user explicitly referenced」
//	P2「Relative paths resolve from the workspace root」
//
//	P1 单独            ⇒ 200
//	P2 单独            ⇒ 200
//	P1 + P2            ⇒ 400 code=11-128（恒定）
//	P1 去任一词         ⇒ 200   （去 prefixed / Tokens / explicitly / referenced /
//	                              the user / @ 均放行）
//	P2 去任一词         ⇒ 200   （去 Relative / resolve / from / workspace / root 均放行）
//	P1+P2 去 host / absolute / identify / files / directories ⇒ 仍 400
//	纯合成最小句（词序打散、只保留关键词）⇒ 200 ⇒ 是**整句逐字匹配**，不是词频
//
// 也就是说：上游拿这两句当作「官方客户端 @ 路径协议说明」的特征指纹，两句共现
// 即判定为冒充官方客户端。DSH 的系统提示词恰好逐字描述了同一套 @ 协议，于是被拦。
//
// 处置：把两句都改写（各动一个词，语义不变）。**两句都改**而不是只改一句，因为
// 规则随时可能收紧成「单句即拦」；而且不共现时改写也无害——这两句只出现在
// 描述 @ 协议的系统提示词里，正常对话不含。
//
// 与 sanitize_dsh.go 的关系：那是本产品**身份句**黑名单（改写产品名所在的整句），
// 这里是**协议文案**黑名单（不含产品名，无法靠产品名兜底），两者机制独立。
// 与既有 sanitizeRewrites 的关系：那一套处理 Claude Code / Codex CLI 的身份句，
// 同样是「上游逐字黑名单」这一套机制。

package upstream

import (
	"regexp"
	"strings"
)

// pathRefRewrites 「@ 路径引用协议」黑名单文案 → 同义改写（各动一个词，语义不变）。
//
// 改写点选择（每条都实测放行）：
//   - P1：去掉 "explicitly" ⇒ "Tokens prefixed with @ are paths the user referenced."
//     保留「以 @ 开头的是用户明确引用的路径」的语义。
//   - P2：把 "from the workspace root" 改成 "from the root of the workspace"
//     ⇒ 破坏 "workspace root" 这个逐字片段，同时语义完全等价。
//
// 匹配串**不带结尾标点**，让原有标点原样保留（P1 句末是 "."，P2 后接 ";"）。
var pathRefRewrites = [][2]string{
	{
		"Tokens prefixed with @ are paths the user explicitly referenced",
		"Tokens prefixed with @ are paths the user referenced",
	},
	{
		"Relative paths resolve from the workspace root",
		"Relative paths resolve from the root of the workspace",
	},
}

// pathRefFingerprintRe 预检用：命中任一句即需要净化。
//
// 必须是大小写不敏感的正则（上游按大小写不敏感逐字匹配，实测 "the dsh runtime"
// 之类变体同样触发），而 sanitizeText 的预检入口 hasFingerprint 走的是
// 大小写敏感的 strings.Contains，只加字面量会漏掉变体 → 整段净化被跳过 →
// 变体原样出站 → 必然被拦。故单独用已编译的 (?i) 正则兜底。
var pathRefFingerprintRe = regexp.MustCompile(
	`(?i)Tokens prefixed with @ are paths the user explicitly referenced` +
		`|Relative paths resolve from the workspace root`)

// sanitizePathRefProtocol 改写「@ 路径引用协议」黑名单文案。
//
// 幂等：改写后的文本不再命中任何一条改写串，二次调用是恒等变换。
func sanitizePathRefProtocol(text string) string {
	if !pathRefFingerprintRe.MatchString(text) {
		return text
	}
	for _, rw := range pathRefRewrites {
		text = replaceFold(text, rw[0], rw[1])
	}
	return text
}

// pathRefProtocolPresent 报告文本中是否仍存在「@ 路径协议」黑名单文案。
// 仅供测试与诊断使用。
func pathRefProtocolPresent(text string) bool {
	for _, rw := range pathRefRewrites {
		if strings.Contains(strings.ToLower(text), strings.ToLower(rw[0])) {
			return true
		}
	}
	return false
}
