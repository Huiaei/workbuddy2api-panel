package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatModelBusyReturns503Retryable 14003 全池繁忙必须报 **503 + model_busy +
// Retry-After**（可重试语义），而不是 400 model_unavailable（不可重试）。
//
// 这是 issue #24 的实案：400 让 DSH / Codex 认为「换模型才有效」于是不再重试，
// 高峰期用户侧表现为「这个模型用不了」，而官方客户端同一时刻还在正常排队。
func TestChatModelBusyReturns503Retryable(t *testing.T) {
	// 关闭「先等一次」的等待（否则本用例会真睡 10s，且遮盖末端映射的断言）。
	old := modelBusyWaitMax
	modelBusyWaitMax = 0
	defer func() { modelBusyWaitMax = old }()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":14003,"msg":"model is busy, please retry later","requestId":"req-14003"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v body=%s", err, rec.Body)
	}
	if resp.Error.Code != "model_busy" {
		t.Errorf("error.code=%q want model_busy", resp.Error.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Errorf("503 model_busy 必须带 Retry-After（客户端据此退避）")
	}
	// 避让必须是**短**的（秒级）：把间歇抖动停成整分钟正是要修的症状。
	st := p.ModelBlockedForRealm("deepseek-v4.1-flash", "global")
	if !st.Blocked || !st.Transient {
		t.Fatalf("应写入瞬时模型级避让: %+v", st)
	}
	if !strings.HasPrefix(st.Reason, "14003") {
		t.Errorf("reason 应以 14003 开头: %q", st.Reason)
	}
	// 账号本身不该被罚（既不禁用也不进账号级冷却）。
	if a, ok := p.Status("g1"); ok {
		if a.Disabled {
			t.Errorf("14003 不得禁用账号")
		}
		if a.Cooling {
			t.Errorf("14003 不得让账号进入账号级冷却，got reason=%q", a.Reason)
		}
	}
}

// TestChatModelBlockedDeterministicKeeps400 确定性原因（11102/11158）必须**保持**
// 400 model_unavailable 且不带 Retry-After —— 换号/等待都必然同样失败。
func TestChatModelBlockedDeterministicKeeps400(t *testing.T) {
	old := modelBusyWaitMax
	modelBusyWaitMax = 0
	defer func() { modelBusyWaitMax = old }()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11102,"msg":"service info not found","requestId":"req-11102"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s rec=%s", rec.Code, rec.Body, rec.Header())
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("确定性模型不可用不该带 Retry-After，got=%q", ra)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Errorf("body 应含 model_unavailable: %s", rec.Body)
	}
}
