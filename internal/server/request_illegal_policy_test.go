package server

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 上游「request illegal」有两种形态，msg 文案**完全相同**，只有业务码不同：
//
//	11-128 / 11128 → 请求级「通道校验失败」，上游明示可重试（"请重新发送"）。
//	11140          → 账号级授权限制（auth/auth_forbidden），要申诉解除。
//
// 二者历史上被同一个 marker（"request illegal"）归为 ErrAccountFault，于是：
//   - 11-128 形态被 Disable → 仍可用的账号被永久踢出池子，用户只能反复点
//     「解冻」（issue #197 / #231 的用户侧症状：官方客户端能聊、面板里却标成
//     banned）；
//   - 一旦把 11-128 改成软冷却，11140 的真封禁也会跟着降级 → 账号永远轮换、
//     永远不恢复。
//
// 故按**结构化业务码**在分类层分野：11-128/11128 → ErrChannelRefused（不罚号），
// 11140 → ErrAccountFault（禁用）。

// channelVerifyBody 上游 11-128 通道校验失败的真实 body（issue #197 用户抓包实录）。
const channelVerifyBody = `{"code":11128,"msg":"request illegal","requestId":"XXXX",` +
	`"displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"},` +
	`"displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client. Please resend from the official client.",` +
	`"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。"}}`

// accountBannedBody 上游 11140 账号级授权限制（auth/auth_forbidden）body。
const accountBannedBody = `{"code":11140,"msg":"request illegal"}`

// TestApplyErrorPolicyChannelRefusedDoesNotDisable 11-128/11128 通道校验失败：
// 账号**不得**被禁用、不得被冷却、不得累计错误计数——账号本身健康。
// 历史实现在此 Disable（issue #197），把仍可用的号永久踢出池子。
func TestApplyErrorPolicyChannelRefusedDoesNotDisable(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 60 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrChannelRefused, channelVerifyBody, "glm-5.2", nil)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号状态缺失")
	}
	if st.Disabled {
		t.Fatalf("11-128 通道校验失败不得禁用账号（issue #197）: %+v", st)
	}
	if st.Cooling {
		t.Fatalf("11-128 通道校验失败不得冷却账号（账号本身健康）: %+v", st)
	}
	if st.ErrTotal != 0 {
		t.Errorf("不罚号路径不应累计错误计数: err_total=%d", st.ErrTotal)
	}
}

// TestApplyErrorPolicyAccountBannedStillDisables 11140 账号级封禁：必须 Disable。
// 回归守卫：若有人把 11-128 的零罚号改动做成"整个 request illegal 都不罚"，
// 真封禁会退化成永远轮换、永远不恢复。
func TestApplyErrorPolicyAccountBannedStillDisables(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 60 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, accountBannedBody, "glm-5.2", nil)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号状态缺失")
	}
	if !st.Disabled {
		t.Fatalf("11140 账号级封禁必须禁用账号: %+v", st)
	}
	if !strings.Contains(strings.ToLower(st.DisabledReason), "request illegal") {
		t.Errorf("disabled_reason=%q 应说明是 request illegal", st.DisabledReason)
	}
}

// TestClassifyRequestIllegalSplitsByBusinessCode 分类层按业务码分流：
// 11-128/11128 → ErrChannelRefused，11140 → ErrAccountFault。
func TestClassifyRequestIllegalSplitsByBusinessCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want upstream.ErrKind
	}{
		{"11-128 通道校验失败", channelVerifyBody, upstream.ErrChannelRefused},
		{"11-128 字符串业务码", `{"code":"11-128","msg":"request illegal"}`, upstream.ErrChannelRefused},
		{"11128 裸体", `{"code":11128,"msg":"request illegal"}`, upstream.ErrChannelRefused},
		{"11140 账号封禁", accountBannedBody, upstream.ErrAccountFault},
		// 11140 的模型级限流变体：必须仍是软限流，不能被误判成账号封禁。
		{"11140 限流变体", `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, upstream.ErrSoftRate},
		// 裸 "request illegal"（**无业务码**）：判不准是通道校验还是账号封禁 ——
		// 归 ErrChannelRefused（**不罚号**、继续轮转）。
		//
		// 旧口径落 ErrClient，而 ErrClient 是**唯一会喂连败计数**的分支 ⇒
		// 5 次连败就把健康号降权出池 10 分钟（审计 D13）。三个可选口径里
		// （通道校验零惩罚 / 账号封禁永久禁用 / ErrClient 降权出池），最轻且
		// 最可自愈的是通道校验；且上游后来把同一句文案升级成带 11-128 业务码
		// 的形态 ⇒ 无码形态极可能是 11-128 的旧版。
		{"裸 request illegal", `{"msg":"request illegal"}`, upstream.ErrChannelRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstream.Classify(400, tc.body); got != tc.want {
				t.Fatalf("Classify=%v want %v", got, tc.want)
			}
		})
	}
}

// TestIsChannelRefused 通道校验失败判定：结构化业务码 + 文案合取。
func TestIsChannelRefused(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"11-128 业务码", `{"code":"11-128","msg":"request illegal"}`, true},
		{"11128 业务码", `{"code":11128,"msg":"request illegal"}`, true},
		{"嵌套业务码", `{"error":{"data":{"code":"11-128","msg":"request illegal"}}}`, true},
		// issue #188 实录：msg 是 "conversation id missing"（同一业务码的变体）。
		{"11128 conversation id missing", `{"code":11128,"msg":"conversation id missing"}`, true},
		// issue #231 实录：首条消息不是 system。
		{"11128 first message is not system", `{"code":11128,"msg":"first message is not system prompt"}`, true},
		// 11140 = 真封禁，不得降级成通道校验。
		{"11140 账号封禁", `{"code":11140,"msg":"request illegal"}`, false},
		{"11140 + 渠道校验文案", `{"code":11140,"msg":"request illegal","displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`, false},
		// 真内容拦截（"blocked by security policy"）仍归审核域：它不带 11-128
		// 业务码（实测录像是纯文案），是**内容**问题，不是请求形状问题。
		{"11128 真内容审核变体", `{"code":11128,"msg":"blocked by security policy"}`, false},
		// 但 11-128 + "unapproved channel"/"illegal api invocation" 是**渠道校验**
		// 变体（2026-10-20 真机实录：首条 developer → 400 11-128 Illegal API
		// invocation from an unapproved channel），不得归审核域：归审核域会触发
		// 全局降级门（降级到次日 00:00 CST），一次请求级形状问题被放大成整网关
		// 整天的降级。
		{"11128 unapproved channel", `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, true},
		{"11128 unapproved channel（短文案）", `{"code":11128,"msg":"unapproved channel"}`, true},
		// 无业务码 → 不认（宁可通用兜底，不可凭文案硬禁用）。
		{"裸 request illegal（无业务码）", `{"msg":"request illegal"}`, false},
		{"无关 body", `{"code":0,"msg":"ok"}`, false},
		{"空 body", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstream.IsChannelRefused(tc.body); got != tc.want {
				t.Fatalf("IsChannelRefused(%s)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestIsAccountRestricted 账号级限制判定：code + 文案双条件。
func TestIsAccountRestricted(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"11140 + request illegal", accountBannedBody, true},
		{"11140 + 限流文案（不得判为封禁）", `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{"11128 + request illegal（通道校验，非封禁）", `{"code":11128,"msg":"request illegal"}`, false},
		{"裸 request illegal（无业务码）", `{"msg":"request illegal"}`, false},
		{"空 body", ``, false},
		// msg 是封禁文案、但 displayMsg 带模型级限流说明：11140 的信封里 msg 与
		// displayMsg 是两个字段，上游可能这么组合。那是模型侧限流（几分钟自愈），
		// 不是账号封禁——永久禁用好号并让用户重新登录是最重的误伤。
		{"11140 + 限流 displayMsg（不得判为封禁）", `{"code":11140,"msg":"request illegal","displayMsg":{"en":"The model provider is rate-limiting requests. Please wait a moment and try again."}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstream.IsAccountRestricted(tc.body); got != tc.want {
				t.Fatalf("IsAccountRestricted(%s)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestClassify11140RateLimitVariantStaysSoftRate 回归守卫：11140 信封里同时带
// "request illegal" 与模型级限流文案时，分类必须落在 ErrSoftRate（软冷却），
// 不得落到 ErrAccountFault（永久禁用）。
//
// 这是本次修复实测抓到的真实误伤：上游把模型级限流文案塞进 11140 的
// displayMsg，msg 仍是 "request illegal"，于是 code+文案双条件双双命中，
// 分类层直接判成账号封禁 → 好号被永久踢出池子。
func TestClassify11140RateLimitVariantStaysSoftRate(t *testing.T) {
	body := `{"code":11140,"msg":"request illegal","displayMsg":{"en":"The model provider is rate-limiting requests. Please wait a moment and try again."}}`
	if got := upstream.Classify(400, body); got != upstream.ErrSoftRate {
		t.Fatalf("Classify=%v want %v（限流文案不得被当成账号封禁）", got, upstream.ErrSoftRate)
	}
	if upstream.IsAccountRestricted(body) {
		t.Fatal("IsAccountRestricted 必须对限流变体返回 false")
	}
}
