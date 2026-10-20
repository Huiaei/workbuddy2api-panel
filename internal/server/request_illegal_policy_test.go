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
//	11-128 / 11128 → 请求级「渠道校验拦截」，上游明示可重试（"请重新发送"）。
//	11140          → 账号级授权封禁（auth/auth_forbidden），要重新登录。
//
// 二者历史上被同一个 marker（"request illegal"）归为 ErrAccountFault，于是：
//   - 11-128 形态被 Disable → 仍可用的账号被永久踢出池子，用户只能反复点
//     「解冻」（issue #197）；
//   - 一旦把 11-128 改成软冷却，11140 的真封禁也会跟着降级 → 账号永远轮换、
//     永远不恢复。
//
// 故按业务码分野：11-128/11128 软冷却（绝不 Disable），11140 保持 Disable。

// channelVerifyBody 上游 11-128 渠道校验拦截的真实 body（issue #197 用户抓包实录）。
const channelVerifyBody = `{"code":11128,"msg":"request illegal","requestId":"XXXX",` +
	`"displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"},` +
	`"displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client. Please resend from the official client.",` +
	`"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。"}}`

// accountBannedBody 上游 11140 账号级授权封禁（auth/auth_forbidden）body。
const accountBannedBody = `{"code":11140,"msg":"request illegal"}`

// TestApplyErrorPolicyChannelVerificationOnlySoftCools 11-128 渠道校验拦截：
// 账号**不得**被禁用，只软冷却。历史实现在此 Disable（issue #197）。
func TestApplyErrorPolicyChannelVerificationOnlySoftCools(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 60 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, channelVerifyBody, "glm-5.2", nil)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号状态缺失")
	}
	if st.Disabled {
		t.Fatalf("11-128 渠道校验拦截不得禁用账号（issue #197）: %+v", st)
	}
	if !st.Cooling {
		t.Fatalf("11-128 渠道校验拦截应进入软冷却: %+v", st)
	}
	if st.CoolKind != "soft_rate" {
		t.Errorf("cool_kind=%q want soft_rate", st.CoolKind)
	}
	if !strings.Contains(st.Reason, "channel verification") {
		t.Errorf("reason=%q 应说明是渠道校验拦截", st.Reason)
	}
}

// TestApplyErrorPolicyAccountBannedStillDisables 11140 账号级封禁：必须 Disable。
// 回归守卫：若有人把 11-128 的软冷却改动做成"整个 request illegal 都软冷却"，
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

// TestIsChannelVerificationBlocked 业务码与文案两路判定。
func TestIsChannelVerificationBlocked(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"11-128 业务码", `{"code":"11-128","msg":"request illegal"}`, true},
		{"11128 业务码", `{"code":11128,"msg":"request illegal"}`, true},
		{"嵌套业务码", `{"error":{"data":{"code":"11-128","msg":"request illegal"}}}`, true},
		{"渠道校验文案（无业务码）", `{"msg":"request illegal","displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`, true},
		{"11140 账号封禁", `{"code":11140,"msg":"request illegal"}`, false},
		{"裸 request illegal（无特征）", `{"code":11140,"msg":"request illegal"}`, false},
		{"无关 body", `{"code":0,"msg":"ok"}`, false},
		{"空 body", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstream.IsChannelVerificationBlocked(tc.body); got != tc.want {
				t.Fatalf("IsChannelVerificationBlocked(%s)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestClassifyRequestIllegalStillAccountFault 两种 request illegal 形态都应
// 归 ErrAccountFault（分类层不区分，分野发生在 applyErrorPolicy）。
func TestClassifyRequestIllegalStillAccountFault(t *testing.T) {
	for _, body := range []string{channelVerifyBody, accountBannedBody} {
		if got := upstream.Classify(400, body); got != upstream.ErrAccountFault {
			t.Errorf("Classify(%s)=%v want ErrAccountFault", body, got)
		}
	}
}
