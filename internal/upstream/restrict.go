package upstream

import "strings"

// 11140 的第二种形态：账号风控「限制文书」（restrict）。
//
// 上游把至少四种语义塞进同一个业务码 11140，只有 **code + 文案双条件** 才能区分：
//   - "request illegal" —— 账号级授权风控（见 accountRestrictedMsgFamily /
//     IsAccountRestricted）：ErrAccountFault + 硬禁用；
//   - 「账号存在异常行为，已被限制使用」这类**限制文书** —— 同样是账号级、
//     同样要禁用，但**不带 "request illegal" 字样**（本文件）；
//   - 模型级限流（见 modelRateLimitFamily / softRateMarkers）—— 必须保持
//     ErrSoftRate，等几分钟自愈；
//   - 内容审核。
//
// 为什么必须单独立一个判据：限制文书不带 "request illegal"，于是 classification
// 会一路滑到通用 4xx 兜底（ErrClient：只换号、不罚号），而请求本身已经进了风控
// 名单——同一个请求顺着轮转挨个号去撞，能把整池健康号在几十秒内全送进风控
// （server/restrict.go 的连坐闸就是为这件事兜底）。
//
// 判据必须**同时**满足 code 与文案：
//   - 只看 code：11140 也承载模型级限流 → 把可自愈的限流号永久禁用；
//   - 只看文案：泛化的「已被限制」不是账号级判据（"该模型已被限制使用" 那种
//     模型级口径会被误判成账号封禁）。
var restrictionMarkers = []string{
	"账号存在异常行为",
	"unusual activity",
}

// IsRestricted 报告上游 body 是否为 11140 的「账号存在异常行为 / unusual
// activity」限制文书形态。
//
// 与 IsAccountRestricted 的分工（两者互不替代，Classify 两处都要判）：
//   - IsAccountRestricted：认 "request illegal" 家族（账号授权风控）；
//   - IsRestricted（本函数）：认限制文书（异常行为 / unusual activity）。
//
// 无状态、幂等，可在 SSE 错误帧上逐帧调用。
func IsRestricted(body string) bool {
	if !has11140(body) {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range restrictionMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// IsAccountRestriction 报告 body 是否为**账号级** 11140 限制（"request illegal"
// 家族与限制文书两种形态之一）。
//
// 供 server 侧连坐计数使用：两种形态都该「命中即停手、禁用该号」，区别只在
// 面板/日志里给出的理由文案（见 server.handler.applyErrorPolicy）。
func IsAccountRestriction(body string) bool {
	if IsRestricted(body) {
		return true
	}
	return has11140(body) && strings.Contains(strings.ToLower(body), accountRestrictedMsg)
}

// has11140 报告 body 是否带业务码 11140。
//
// 结构化解析优先（hasBusinessCode 只认严格 JSON 并递归遍历业务码键）；字面形态
// 兜底（codeMarker 认 `"code":11140` 这类非严格 JSON 的写法）。两层都要：上游
// 偶有把 code 塞进半截 JSON / 非标准信封的形态，只靠结构化解析会漏判，而漏判
// 的代价是把一个已被限制的号当成健康号继续轮换。
func has11140(body string) bool {
	if hasBusinessCode(body, "11140") {
		return true
	}
	return codeMarker(strings.ToLower(body), "11140")
}
