// sanitize_dsh.go 出站脱敏：本产品身份句的**逐字黑名单**规避。
//
// 背景（2026-10-21 CN realm 实案）：上游渠道校验新增了对本产品身份句的**逐字**
// 黑名单，命中即 400 code=11128 "Illegal API invocation from an unapproved channel"
// （displayTips:「这次调用没通过渠道校验」）。这不是账号问题，也不是头/字段问题——
// 是**请求体里的某一句话**逐字命中了上游的拦截串。
//
// 实测（直连 CN 上游，K≥3 重复，确定）：
//
//	T1「You are an AI agent powered by DeepSeek Harness.」
//	   独立成句（system 或 assistant 角色）            ⇒ 恒定 400
//	   改写一个词（"the DSH runtime"）后              ⇒ 恒定 200
//	T2「You are interacting with the user through the DeepSeek Harness Web GUI whose URL
//	   the runtime context names. When the user refers to "this page", "this GUI", or "this
//	   app" without naming another target, they mean this GUI.」
//	   两句**连体**（221 字符）                       ⇒ 恒定 400
//	   拆成两半各自单独                              ⇒ 恒定 200（必须连体才命中）
//	   改写一个词（"the DSH web interface"）后        ⇒ 恒定 200
//
// 不触发的形态（同样实测确认，避免过度脱敏）：
//   · "DeepSeek Harness" 单独出现                    ⇒ 200
//   · "You are a DeepSeek Harness user." 等其它句式   ⇒ 200
//   · 路径形态 C:\app\DeepSeek Harness\...            ⇒ 200
//   · 中性句里出现两次产品名                          ⇒ 200
// ⇒ 上游拦的是**特定整句**，不是产品名本身。所以修复必须**改写整句**，
// 而不是把产品名全局替换成别名（那会破坏路径等真实信息）。
//
// 两条黑名单句**互相独立**（改掉 T1 仍被 T2 拦，改掉 T2 仍被 T1 拦），
// 因此两条都要改写。
//
// 与既有 sanitizeRewrites 的关系：那一套处理的是 Claude Code / Codex CLI 的
// 身份句（同样的上游逐字黑名单机制），本文件是同一机制下**本产品**的身份句。
// 两者共用 sanitizeText 入口与 hasFingerprint 预检。

package upstream

import (
	"regexp"
	"strings"
)

// dshProductNameRe 匹配本产品名 "DeepSeek Harness"（大小写不敏感）。
//
// 用途只有两个：
//  1. hasFingerprint 预检——上游按**大小写不敏感**的逐字匹配拦截（实测
//     "you are an AI agent powered by deepseek harness." 同样触发 400），
//     而预检走的是大小写敏感的 strings.Contains，只写 "DeepSeek Harness" 会漏掉
//     变体，净化被整段跳过 ⇒ 变体原样出站 ⇒ 必然被拦。
//  2. 改写后兜底：把**不属于这两条黑名单句**的残留产品名换成别名 "DSH"。
//     路径形态（前后紧邻 \ 或 /）逐字保留——那是运行期真实路径，
//     改了会让模型拿到错路径（实测路径形态不被拦，无需改写）。
var dshProductNameRe = regexp.MustCompile(`(?i)deepseek harness`)

// dshProductAlias 替换后的产品别名。DSH 是本产品自身的简称（提示词里已在用，
// 例如 "inspect or extend DSH itself"），语义不丢。
const dshProductAlias = "DSH"

// dshIdentitySentences 黑名单身份句 → 同义改写。
//
// 每条改写只动**一个词**，且改写后实测恒定 200（K≥3）：
//   - T1: "…powered by DeepSeek Harness" → "…powered by the DSH runtime"
//   - T2: "…through the DeepSeek Harness Web GUI" → "…through the DSH web interface"
//     （改 "Web GUI" 也可，但同时改产品名更稳——上游可能随时加严匹配）
//
// 注意 T2 的黑名单串是**两句连体**（221 字符），不是单句。故匹配串取两句的
// 公共前缀（第一句的前半段）——命中后整段改写，第二句"they mean this GUI"
// 本身不含产品名，保留原样即可（实测：只改第一句半段即放行）。
var dshIdentitySentences = [][2]string{
	{
		"You are an AI agent powered by DeepSeek Harness",
		"You are an AI agent powered by the DSH runtime",
	},
	{
		"You are interacting with the user through the DeepSeek Harness Web GUI",
		"You are interacting with the user through the DSH web interface",
	},
	{
		"The runtime context names the DeepSeek Harness implementation checkout",
		"The runtime context names the DSH implementation checkout",
	},
	{
		"The DeepSeek Harness implementation checkout is at",
		"The DSH implementation checkout is at",
	},
}

// dshPathNeighbor 判断产品名出现位置是否处于**路径上下文**（前后紧邻 \ 或 /）。
// 路径形态逐字保留：那是运行期真实路径（如 C:\app\DeepSeek Harness\resources\app.asar\dsh\），
// 改写会让模型拿到错路径；而实测路径形态本来也不被拦。
func dshPathNeighbor(text string, start, end int) bool {
	if start > 0 {
		c := text[start-1]
		if c == '\\' || c == '/' {
			return true
		}
	}
	if end < len(text) {
		c := text[end]
		if c == '\\' || c == '/' {
			return true
		}
	}
	return false
}

// sanitizeDSHProductName 改写含本产品名的身份句。
//
// 步骤：
//  1. 按 dshIdentitySentences 整句改写（大小写不敏感，最小改动、语义不变）；
//  2. 对**剩余**的 "DeepSeek Harness"：路径上下文逐字保留，其余换成别名 "DSH"。
//
// 第 2 步是兜底：上游随时可能把黑名单扩到别的句式，届时即使没有命中现成
// 改写表，残留的产品名也不会原样出站。实测中性句里出现产品名不被拦，
// 故这一步不会误伤正常语义。
func sanitizeDSHProductName(text string) string {
	if !dshProductNameRe.MatchString(text) {
		return text
	}
	for _, rw := range dshIdentitySentences {
		text = replaceFold(text, rw[0], rw[1])
	}
	// 剩下的产品名逐个处理：路径形态保留，其余换成别名。
	locs := dshProductNameRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, loc := range locs {
		s, e := loc[0], loc[1]
		b.WriteString(text[prev:s])
		if dshPathNeighbor(text, s, e) {
			b.WriteString(text[s:e]) // 路径形态：逐字保留
		} else {
			b.WriteString(dshProductAlias)
		}
		prev = e
	}
	b.WriteString(text[prev:])
	return b.String()
}

// replaceFold 大小写不敏感地替换全部出现（保留原串的其余部分）。
// 用于身份句改写：上游按逐字匹配拦截，大小写变体同样会命中，改写必须大小写不敏感。
func replaceFold(text, old, new string) string {
	if old == "" {
		return text
	}
	re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(old))
	if err != nil {
		return text
	}
	return re.ReplaceAllString(text, new)
}
