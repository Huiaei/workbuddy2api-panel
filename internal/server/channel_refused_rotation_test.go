package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 11-128 / 11128「通道校验失败」的轮转语义（issue #197 / #188 / #231）。
//
// 上游的风控是**按账号**判定的：issue #197 的实测（用与网关完全一致的出站头部
// 直连上游）显示，同一时刻被记成「禁用」的号稳定 400 11-128，而**另一个健康号
// 稳定 200**。所以这类错误必须**继续轮转**——轮转可能换到能过的号。
//
// 早期实现在这里 fail-fast（立即回 400），等于把「池里明明还有健康号」的请求
// 直接丢掉：实测 3 个健康号、只有第 1 个撞 11-128 时，fail-fast 版 status=400
// 且只打了 1 次上游；轮转版 status=200 且打了 2 次。
//
// 同时它**不罚号**（applyErrorPolicy 对 ErrChannelRefused 零动作）：账号本身
// 健康，冷却/禁用都是误伤——被禁用的号在官方客户端里能正常聊天。

// channelRefusedBody 上游 11-128 通道校验失败的真实 body（issue #197 抓包）。
const channelRefusedBody = `{"code":11128,"msg":"request illegal","requestId":"XXXX",` +
	`"displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"},` +
	`"displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client. Please resend from the official client."}}`

// TestChannelRefusedRotatesToHealthyAccount 池里第 1 个号撞 11-128、第 2 个号
// 正常 → 必须轮转到第 2 个号并成功返回；**不得**在第 1 次失败时就回 400。
func TestChannelRefusedRotatesToHealthyAccount(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if calls == 1 {
			return http.StatusBadRequest, channelRefusedBody, false
		}
		return http.StatusOK, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", Domain: "www.workbuddy.ai", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", Domain: "www.workbuddy.ai", AccessToken: "at-2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（应轮转到健康号）body=%s", rec.Code, rec.Body)
	}
	if calls < 2 {
		t.Fatalf("calls=%d want >=2（必须轮转，不能在首次 11-128 就放弃）", calls)
	}
	// 11-128 不罚号：两个号都不得被冷却/禁用。
	for _, uid := range []string{"u1", "u2"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("账号 %s 状态缺失", uid)
		}
		if st.Disabled {
			t.Errorf("账号 %s 被禁用（11-128 是请求级问题，账号健康）: %+v", uid, st)
		}
		if st.Cooling {
			t.Errorf("账号 %s 被冷却（11-128 不得罚号）: %+v", uid, st)
		}
	}
}

// TestChannelRefusedExhaustedRotationReturns400 所有号都撞 11-128 → 轮转耗尽后
// 回 **400 + channel_refused**（不是 503 no_healthy_account——后者把确定失败的
// 请求伪装成「服务端过载」，客户端会无限重试），且全程不罚号。
func TestChannelRefusedExhaustedRotationReturns400(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return http.StatusBadRequest, channelRefusedBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", Domain: "www.workbuddy.ai", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", Domain: "www.workbuddy.ai", AccessToken: "at-2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
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
	if resp.Error.Code != "channel_refused" {
		t.Errorf("error.code=%q want channel_refused", resp.Error.Code)
	}
	if calls < 2 {
		t.Errorf("calls=%d want >=2（应尝试所有可用号）", calls)
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.Disabled || st.Cooling {
			t.Errorf("账号 %s 被惩罚（11-128 不得罚号）: %+v", uid, st)
		}
	}
}

// TestUnapprovedChannelVariantIsNotContentBlocked 11-128 + "unapproved channel"
// 是**渠道校验**变体（2026-10-20 真机实录：首条 developer → 400 11-128
// Illegal API invocation from an unapproved channel），不得归 ErrContentBlocked：
// 归审核域会触发全局降级门（h.degrade.Trigger()，降级到次日 00:00 CST）——
// 一次请求级形状问题被放大成整网关整天的降级。
func TestUnapprovedChannelVariantIsNotContentBlocked(t *testing.T) {
	body := `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return http.StatusBadRequest, body, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", Domain: "www.workbuddy.ai", AccessToken: "at-1", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v body=%s", err, rec.Body)
	}
	if resp.Error.Code == "content_blocked" {
		t.Errorf("11-128 渠道校验变体不得归 content_blocked（会触发全局降级门）: %s", rec.Body)
	}
	if h.degrade.Active() {
		t.Error("11-128 渠道校验不得触发全局降级门")
	}
}
