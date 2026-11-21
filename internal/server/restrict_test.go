package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// restrictBody 是上游 11140 限制文书的真实形态（带 requestId——申诉要用，
// 故服务端必须原文照透）。
const restrictBody = `{"code":11140,"msg":"账号存在异常行为，已被限制使用","requestId":"aaaabbbbccccdddd"}`

// restrictFrame 是同一份限制文书的 SSE 形态：观察者只认带顶层 error 键的帧。
const restrictFrame = `{"error":{"code":11140,"message":"账号存在异常行为，已被限制使用"}}`

// postChat 发一个最小 chat 请求（非流式）。
func postChat(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	return postChatBody(t, h, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
}

// postChatStream 发一个最小 chat 流式请求。
func postChatStream(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	return postChatBody(t, h, `{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
}

func postChatBody(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body))))
	return rec
}

// disabledCount 返回池中被硬禁用的号数（连坐阈值与「命中即禁用」断言共用）。
func disabledCount(t *testing.T, p *pool.Pool) int {
	t.Helper()
	_, _, _, disabled, _ := p.CountsDetailed()
	return disabled
}

// cnAccounts 造两个 CN 账号（i1 积分更高 → 确定性随机源先选中它）。
func cnAccounts(t *testing.T) *pool.Pool {
	t.Helper()
	p := testPoolWith(
		&auth.Auth{UID: "i1", AccessToken: "at-i1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i2", AccessToken: "at-i2", ExpiresAt: 9999999999},
	)
	p.SetCredits("i1", 2000, 0)
	p.SetCredits("i2", 1000, 0)
	return p
}

// TestRestrictionHitStopsRotation 第一道闸（停手）：上游回 11140 限制文书 ⇒ 该号
// 硬禁用 + 本请求**不再换号**（一次请求只打一次上游），响应 503 +
// account_restricted_hold 并原文照透（含 requestId）。
//
// 反例（修复前的行为）：限制文书落到通用 4xx 兜底 → ErrClient → 只换号不罚号 →
// 同一请求顺着轮转把第二个号也打成 11140（本测试的第二条断言正是钉住这一点：
//
//	i2 必须完好，calls 必须等于 1）。
func TestRestrictionHitStopsRotation(t *testing.T) {
	p := cnAccounts(t)
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return http.StatusForbidden, restrictBody, false
	})
	h := NewHandler(Config{
		Pool:               p,
		Upstream:           up,
		MaxRotate:          3,
		RestrictionHold:    30 * time.Minute,
		RestrictionWaveDiv: 10,
	})

	rec := postChat(t, h, "glm-5.2")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s want 503 account_restricted_hold", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "account_restricted_hold") {
		t.Fatalf("body=%s want code account_restricted_hold", body)
	}
	if !strings.Contains(body, "aaaabbbbccccdddd") {
		t.Errorf("body=%s want upstream body verbatim (requestId is needed to appeal)", body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1: a restricted account must stop rotation, not burn the next account", calls)
	}
	if got := disabledCount(t, p); got != 1 {
		t.Errorf("disabled=%d want 1 (the flagged account)", got)
	}
	if st, ok := p.Status("i1"); !ok || !st.Disabled || !strings.Contains(st.DisabledReason, "unusual activity") {
		t.Errorf("i1 status=%+v want Disabled with reason mentioning unusual activity", st)
	}
	if st, ok := p.Status("i2"); !ok || st.Disabled {
		t.Errorf("i2 status=%+v want healthy (must not be implicated by a stopped request)", st)
	}
	// 阈值 2、只封了 1 个 → 连坐闸未触发，派发照常。
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Errorf("restrict.remaining(cn)=%v want 0 (threshold not reached)", d)
	}
}

// TestRestrictionWavePausesRealm 第二道闸（连坐）：同域封到阈值 ⇒ 暂停该域派发，
// 暂停期内请求在入口就被 503 拦下（Retry-After 非空），**不消耗任何账号**。
func TestRestrictionWavePausesRealm(t *testing.T) {
	p := cnAccounts(t)
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return http.StatusForbidden, restrictBody, false
	})
	h := NewHandler(Config{
		Pool:               p,
		Upstream:           up,
		MaxRotate:          3,
		RestrictionHold:    30 * time.Minute,
		RestrictionWaveDiv: 10,
	})

	// 第 1 次：i1 命中 → 停手；阈值 2 未到。
	if rec := postChat(t, h, "glm-5.2"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("1st code=%d body=%s", rec.Code, rec.Body)
	}
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Fatalf("after 1st hit remaining=%v want 0", d)
	}
	// 第 2 次：i2 命中 → 同一域封到 2 个 → 触发连坐暂停。
	if rec := postChat(t, h, "glm-5.2"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("2nd code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2", calls)
	}
	if d := h.restrict.remaining("cn"); d <= 0 {
		t.Fatalf("after wave remaining=%v want > 0", d)
	}
	// 第 3 次：闸门先拦，不打上游。
	rec := postChat(t, h, "glm-5.2")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("3rd code=%d body=%s want 503", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "account_restricted_hold") {
		t.Fatalf("body=%s want code account_restricted_hold", body)
	}
	if !strings.Contains(body, "dispatch paused") {
		t.Errorf("body=%s want the 'dispatch paused' wording (this is the hold gate, not an account verdict)", body)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("Retry-After empty: clients must back off for the hold window")
	}
	if calls != 2 {
		t.Errorf("upstream calls=%d want 2: requests during the hold must not touch upstream", calls)
	}
}

// TestRestrictionHoldRealmIsolation 连坐按域计数：cn 被暂停不影响 global
// （两套独立上游/风控名单，一起暂停等于人为放大故障面）。
func TestRestrictionHoldRealmIsolation(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(prev) })

	p := testPoolWith(
		&auth.Auth{UID: "i1", AccessToken: "at-i1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i2", AccessToken: "at-i2", ExpiresAt: 9999999999},
		// Domain 后缀 .workbuddy.ai ⇒ Realm()=="global"，只服务 global: 模型。
		&auth.Auth{UID: "g1", AccessToken: "at-g1", ExpiresAt: 9999999999, Domain: "acme.workbuddy.ai"},
	)
	p.SetCredits("i1", 3000, 0)
	p.SetCredits("i2", 2000, 0)
	p.SetCredits("g1", 1000, 0)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		switch {
		case strings.Contains(authz, "at-g1"):
			return http.StatusOK, sseOK, true
		default:
			return http.StatusForbidden, restrictBody, false
		}
	})
	h := NewHandler(Config{
		Pool:               p,
		Upstream:           up,
		MaxRotate:          3,
		RestrictionHold:    30 * time.Minute,
		RestrictionWaveDiv: 10,
	})

	for i := 1; i <= 2; i++ {
		if rec := postChat(t, h, "glm-5.2"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("cn #%d code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if d := h.restrict.remaining("cn"); d <= 0 {
		t.Fatalf("cn remaining=%v want > 0", d)
	}
	if d := h.restrict.remaining("global"); d != 0 {
		t.Fatalf("global remaining=%v want 0 (realms are counted separately)", d)
	}
	// global 请求照常派发到 global 号。
	rec := postChatStream(t, h, "global:glm-5.2")
	if rec.Code != http.StatusOK {
		t.Fatalf("global code=%d body=%s want 200 (cn hold must not block global)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "chat.completion.chunk") {
		t.Errorf("global body=%s want streamed completion", rec.Body)
	}
}

// TestRestrictionHoldExpires 暂停到期即恢复派发（退避是「等一会儿」而不是
// 「永久停服」）：到期的域重新选号，健康号照常服务。
func TestRestrictionHoldExpires(t *testing.T) {
	oldBase, oldMax := restrictionHoldBase, restrictionHoldMax
	restrictionHoldBase = 30 * time.Millisecond
	restrictionHoldMax = 30 * time.Millisecond
	t.Cleanup(func() { restrictionHoldBase, restrictionHoldMax = oldBase, oldMax })

	p := testPoolWith(
		&auth.Auth{UID: "i1", AccessToken: "at-i1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i2", AccessToken: "at-i2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i3", AccessToken: "at-i3", ExpiresAt: 9999999999},
	)
	p.SetCredits("i1", 3000, 0)
	p.SetCredits("i2", 2000, 0)
	p.SetCredits("i3", 1000, 0)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at-i3") {
			return http.StatusOK, sseOK, true
		}
		return http.StatusForbidden, restrictBody, false
	})
	h := NewHandler(Config{
		Pool:               p,
		Upstream:           up,
		MaxRotate:          3,
		RestrictionHold:    30 * time.Minute, // 配置更长，但退避起点更短 → 按退避
		RestrictionWaveDiv: 10,
	})

	for i := 1; i <= 2; i++ {
		if rec := postChat(t, h, "glm-5.2"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("hit #%d code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if d := h.restrict.remaining("cn"); d <= 0 {
		t.Fatalf("remaining=%v want > 0 right after the wave", d)
	}
	// 暂停期内：503 且不打上游。
	if rec := postChat(t, h, "glm-5.2"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("held code=%d body=%s want 503", rec.Code, rec.Body)
	}
	time.Sleep(60 * time.Millisecond)
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Fatalf("remaining=%v want 0 after expiry", d)
	}
	// 到期后恢复：只剩健康号 i3，请求照常成功。
	rec := postChatStream(t, h, "glm-5.2")
	if rec.Code != http.StatusOK {
		t.Fatalf("after expiry code=%d body=%s want 200", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "chat.completion.chunk") {
		t.Errorf("after expiry body=%s want streamed completion", rec.Body)
	}
}

// TestRestrictionWaveDivZeroDisablesGate 连坐除数 = 0 ⇒ 关闭第二道闸：照常计数
// 打日志，但永不暂停派发；第一道闸（停手 + 禁用）仍然生效。
func TestRestrictionWaveDivZeroDisablesGate(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "i1", AccessToken: "at-i1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i2", AccessToken: "at-i2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i3", AccessToken: "at-i3", ExpiresAt: 9999999999},
	)
	p.SetCredits("i1", 3000, 0)
	p.SetCredits("i2", 2000, 0)
	p.SetCredits("i3", 1000, 0)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at-i3") {
			return http.StatusOK, sseOK, true
		}
		return http.StatusForbidden, restrictBody, false
	})
	h := NewHandler(Config{
		Pool:               p,
		Upstream:           up,
		MaxRotate:          3,
		RestrictionHold:    30 * time.Minute,
		RestrictionWaveDiv: 0,
	})

	for i := 1; i <= 2; i++ {
		if rec := postChat(t, h, "glm-5.2"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("hit #%d code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Fatalf("remaining=%v want 0 (div=0 turns the wave gate off)", d)
	}
	if got := disabledCount(t, p); got != 2 {
		t.Errorf("disabled=%d want 2 (the first gate still disables each hit)", got)
	}
	if rec := postChatStream(t, h, "glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("after 2 hits code=%d body=%s want 200: div=0 must not pause dispatch", rec.Code, rec.Body)
	}
}

// TestRestrictionHoldZeroKeepsOnlyFirstGate restriction_hold_seconds = 0 ⇒ 只保留
// 「命中即停手」：禁用该号、本请求不换号，但永不暂停该域派发。
func TestRestrictionHoldZeroKeepsOnlyFirstGate(t *testing.T) {
	p := cnAccounts(t)
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return http.StatusForbidden, restrictBody, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3, RestrictionHold: 0, RestrictionWaveDiv: 10})

	rec := postChat(t, h, "glm-5.2")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "account_restricted_hold") {
		t.Fatalf("code=%d body=%s want 503 account_restricted_hold", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1", calls)
	}
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Fatalf("remaining=%v want 0 (hold=0 keeps only the stop-here gate)", d)
	}
	if got := disabledCount(t, p); got != 1 {
		t.Errorf("disabled=%d want 1", got)
	}
}

// TestChatRequestIllegalWithoutCodeKeepsRotating 请求级口径零回归：裸
// "request illegal"（无 11140 码）是通道校验失败 → 照常换号，不罚号、不连坐。
func TestChatRequestIllegalWithoutCodeKeepsRotating(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "i1", AccessToken: "at-i1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i2", AccessToken: "at-i2", ExpiresAt: 9999999999},
	)
	p.SetCredits("i1", 2000, 0)
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return http.StatusForbidden, `{"msg":"request illegal"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3, RestrictionHold: time.Minute, RestrictionWaveDiv: 10})

	rec := postChat(t, h, "glm-5.2")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400 channel_refused", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Errorf("calls=%d want 2 (request-level error must keep rotating)", calls)
	}
	if got := disabledCount(t, p); got != 0 {
		t.Errorf("disabled=%d want 0 (uncoded request illegal is not an account ban)", got)
	}
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Errorf("remaining=%v want 0", d)
	}
}

// TestChatStreamHeadRestrictionDisablesAccount 流式首帧否决：error 帧里的限制
// 文书同样要罚号（帧原文照透给客户端），且这一跳不再换号。
func TestChatStreamHeadRestrictionDisablesAccount(t *testing.T) {
	p := cnAccounts(t)
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return http.StatusOK, "data: " + restrictFrame + "\n\n", true
	})
	h := NewHandler(Config{
		Pool:               p,
		Upstream:           up,
		MaxRotate:          3,
		RestrictionHold:    30 * time.Minute,
		RestrictionWaveDiv: 10,
	})

	rec := postChatStream(t, h, "glm-5.2")
	// 头已写出（SSE 200），状态修正只体现在请求日志里。
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200 (SSE headers already flushed)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "账号存在异常行为") {
		t.Errorf("body=%s want the upstream error frame passed through verbatim", rec.Body)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1", calls)
	}
	if got := disabledCount(t, p); got != 1 {
		t.Errorf("disabled=%d want 1 (stream-side restriction must punish the account too)", got)
	}
}

// TestChatStreamRestrictionCountsTowardWave 流式那一跳虽然不能再换号，但命中必须
// 计入同域连坐：否则流式客户端可以无限次撞进同一波风控而闸门永远不关（观察者只
// 在帧带顶层 error 时触发，这条链路最容易漏掉记账）。
func TestChatStreamRestrictionCountsTowardWave(t *testing.T) {
	p := cnAccounts(t)
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return http.StatusOK, "data: " + restrictFrame + "\n\n", true
	})
	h := NewHandler(Config{
		Pool:               p,
		Upstream:           up,
		MaxRotate:          3,
		RestrictionHold:    30 * time.Minute,
		RestrictionWaveDiv: 10,
	})

	for i := 1; i <= 2; i++ {
		if rec := postChatStream(t, h, "glm-5.2"); rec.Code != http.StatusOK {
			t.Fatalf("stream #%d code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if got := disabledCount(t, p); got != 2 {
		t.Fatalf("disabled=%d want 2 (each streamed restriction bans its account)", got)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}
	if d := h.restrict.remaining("cn"); d <= 0 {
		t.Fatalf("remaining=%v want > 0: stream-side hits must count toward the realm wave", d)
	}
	// 连坐已触发：后续请求在入口被拦，不再打上游。
	rec := postChat(t, h, "glm-5.2")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "dispatch paused") {
		t.Fatalf("code=%d body=%s want 503 dispatch paused", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Errorf("calls=%d want 2 (hold must not touch upstream)", calls)
	}
}

// TestApplyErrorPolicyRestrictionDocument 限制文书（403）⇒ 硬禁用，理由点出
// "unusual activity"，与 "request illegal" 家族分开（用户按这个说法申诉才对得上）。
func TestApplyErrorPolicyRestrictionDocument(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 60 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, restrictBody, "glm-5.2", nil)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if !st.Disabled {
		t.Fatalf("status=%+v want Disabled", st)
	}
	if !strings.Contains(st.DisabledReason, "unusual activity") {
		t.Errorf("reason=%q want it to mention unusual activity", st.DisabledReason)
	}
	if st.Cooling {
		t.Errorf("status=%+v want no cooldown on a hard ban", st)
	}
}

// TestApplyErrorPolicyRestrictionDocumentOn429 429 + 限制文书 ⇒ 只软冷却，不永久
// 禁用（既有分级：上游给了 429 就是「限流、几分钟自愈」，误禁不可自愈）。
func TestApplyErrorPolicyRestrictionDocumentOn429(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 60 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, restrictBody, "glm-5.2", &upstream.Error{Status: http.StatusTooManyRequests})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if st.Disabled {
		t.Fatalf("status=%+v want NOT disabled (429 stays a soft cooldown)", st)
	}
	if !st.Cooling {
		t.Errorf("status=%+v want cooling", st)
	}
}

// TestApplyErrorPolicyRequestIllegalStillDisables 既有形态零回归：11140 +
// "request illegal" 仍是硬禁用，理由文案不变。
func TestApplyErrorPolicyRequestIllegalStillDisables(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 60 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"code":11140,"msg":"request illegal"}`, "glm-5.2", nil)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if !st.Disabled {
		t.Fatalf("status=%+v want Disabled", st)
	}
	if !strings.Contains(st.DisabledReason, "request illegal") {
		t.Errorf("reason=%q want it to mention request illegal", st.DisabledReason)
	}
}

// TestApplyErrorPolicyModelLevelRestrictionKeepsRotating 模型级「该模型已被限制
// 使用」不是账号级判据：不罚号（等价于请求级），轮转照旧。
func TestApplyErrorPolicyModelLevelRestrictionKeepsRotating(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 60 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"code":11140,"msg":"该模型已被限制使用"}`, "glm-5.2", nil)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if st.Disabled {
		t.Fatalf("status=%+v want NOT disabled (model-level wording is not an account ban)", st)
	}
}
