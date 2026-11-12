package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// upstreamBody11134 上游 11134「模型供应商过载」的实测响应体（业务码 + 文案 +
// requestId + retry_policy）。retry_policy.retry_after 是 body 侧的等待窗口来源。
const upstreamBody11134 = `{"code":11134,"msg":"the model provider is temporarily unavailable, please retry later or switch to another model","requestId":"01a1b4d1b3f45f4b99c406eb04d68909","retry_policy":{"retry":true,"retry_after":8}}`

// TestChatProviderOverload11134DoesNotPenalizeOrRotate 11134「模型供应商过载」
// 是**供应商容量**信号，不是账号信号。端到端断言：
//   - 不轮转（只打一次上游：换号仍是打同一个过载供应商，白耗在途名额）；
//   - 不罚号（无冷却/无禁用/无熔断计数/无 errTotal/无连败）——原口径落 ErrServer
//     会喂 Pool.NoteError(uid)，连错 3 次即 30 分钟熔断，把健康号集体打冷；
//   - fail-fast 503 + upstream_busy + 透传上游原文；
//   - body 侧 retry_policy.retry_after 被解析成 Retry-After 响应头；
//   - 会话粘性绑定**保留**（解绑会让客户端重试换号 ⇒ 前缀缓存从零重算）。
func TestChatProviderOverload11134DoesNotPenalizeOrRotate(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 500, upstreamBody11134, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "busy1", AccessToken: "at-busy1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "busy2", AccessToken: "at-busy2", ExpiresAt: 9999999999},
	)
	p.SetCredits("busy1", 2000, 0) // 确定性源 r=0 → 先选 busy1
	p.SetCredits("busy2", 1000, 0)

	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"busy1", "busy2"} },
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	// 预绑定：会话已钉在 busy1 上（模拟历史粘性）。
	sess.Bind("conv-1", "busy1")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503: provider overload is a capacity signal, client should back off)",
			rec.Code, rec.Body)
	}
	// 零轮转：换号仍是打同一个过载供应商。
	if calls["Bearer at-busy1"] != 1 || calls["Bearer at-busy2"] != 0 {
		t.Errorf("calls=%v want busy1 1 次、busy2 0 次（零轮转）", calls)
	}
	// 账号零惩罚：无冷却、无禁用、无熔断计数、无 errTotal、无连败。
	s1, _ := p.Status("busy1")
	if s1.Cooling || s1.Disabled || s1.ErrTotal != 0 || s1.BreakerFails != 0 || s1.ConsecutiveFails != 0 {
		t.Errorf("11134 must not penalize account: %+v", s1)
	}
	// 响应体：upstream_busy code + 上游原文（业务码与文案都在场）。
	body := rec.Body.String()
	if !strings.Contains(body, "upstream_busy") {
		t.Errorf("503 body should carry upstream_busy code: %s", body)
	}
	if !strings.Contains(body, "11134") {
		t.Errorf("503 message should carry upstream 11134 info: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("provider overload must not be reported as account exhaustion: %s", body)
	}
	// body 侧 retry_after → Retry-After 响应头（秒）。
	if got := rec.Header().Get("Retry-After"); got != "8" {
		t.Errorf("Retry-After=%q want %q (body retry_policy.retry_after must be honored)", got, "8")
	}
	// 会话粘性保留（不解绑）。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "busy1" {
		t.Fatalf("sticky binding must be kept on provider overload, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
	if st.delCnt != 0 {
		t.Errorf("sticky binding must not be deleted on provider overload, delCnt=%d", st.delCnt)
	}
}
