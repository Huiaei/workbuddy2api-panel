package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestQueueFullCarriesUpstreamRetryAfter 6021 queue.full 的 503 响应必须把上游
// 明示的 Retry-After 头带给客户端。
//
// 理由：这一路是**渠道级**容量满载（不是账号问题），客户端唯一的正确动作就是
// 等上游窗口过去再重试。上游给了真实等待窗口就原样透传，SDK 才能用上游的
// 节奏退避；自己编一个数反而更糟。
//
// 边界：上游没给 Retry-After 时**不得**编造（宁缺勿滥）。
func TestQueueFullCarriesUpstreamRetryAfter(t *testing.T) {
	newUp := func(retryAfter string) *upstream.Client {
		return &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := http.Header{"Content-Type": []string{"application/json"}}
				if retryAfter != "" {
					h.Set("Retry-After", retryAfter)
				}
				return &http.Response{
					StatusCode: 429,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader(`{"code":6021,"msg":"queue.full.title"}`)),
				}, nil
			})},
			ChatBaseCN:    "https://fake.example",
			BillingBaseCN: "https://fake.example",
		}
	}
	newReq := func() *http.Request {
		return httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	}

	// 上游给了 5 秒 → 响应必须带 Retry-After: 5。
	p := testPoolWith(&auth.Auth{UID: "ra1", AccessToken: "at-ra1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newUp("5")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newReq())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After=%q want %q (上游明示的等待窗口必须透传给客户端)", got, "5")
	}

	// 上游没给 → 不得编造（臆造等待时长比不给更糟）。
	p2 := testPoolWith(&auth.Auth{UID: "ra2", AccessToken: "at-ra2", ExpiresAt: 9999999999})
	h2 := NewHandler(Config{Pool: p2, Upstream: newUp("")})
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, newReq())
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503)", rec2.Code, rec2.Body)
	}
	if got := rec2.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After=%q want empty (上游未明示时不得编造等待时长)", got)
	}
}
