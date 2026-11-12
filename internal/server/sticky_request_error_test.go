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

// TestChatPromptTooLongKeepsBinding prompt 超长（11115）同样是请求级错误：
// 绑定保留（解绑只会让客户端重试换号，缓存从零重算），账号零惩罚，零轮转。
//
// 这是 failKeepBinding 的守护用例：把 failKeepBinding 改回 fail（会解绑粘性）
// ⇒ 本用例失败。
func TestChatPromptTooLongKeepsBinding(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11115,"msg":"prompt is too long"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 2000, 0)
	p.SetCredits("u2", 1000, 0)

	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"u1", "u2"} },
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "u1")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	if calls["Bearer at-u1"] != 1 || calls["Bearer at-u2"] != 0 {
		t.Errorf("calls=%v want u1 1 次、u2 0 次（零轮转）", calls)
	}
	s1, _ := p.Status("u1")
	if s1.Cooling || s1.Disabled || s1.ErrTotal != 0 || s1.BreakerFails != 0 {
		t.Errorf("prompt-too-long must not penalize account: %+v", s1)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "u1" {
		t.Fatalf("sticky binding must be kept on prompt-too-long, got %s ok=%v", uid, ok)
	}
	if st.delCnt != 0 {
		t.Errorf("sticky binding must not be deleted on prompt-too-long, delCnt=%d", st.delCnt)
	}
}
