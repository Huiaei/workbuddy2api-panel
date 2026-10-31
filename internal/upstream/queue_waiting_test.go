package upstream

import (
	"net/http"
	"testing"
)

// TestClassifyQueueWaitingIsUpstreamBusy 6020「queue.waiting.title」必须与 6021
// 同判为渠道级排队（ErrUpstreamBusy），不能落进 429 兜底变成 ErrSoftRate。
//
// 背景：6020 之前没有单独判，429 直接落 ErrSoftRate → 账号级软冷却 600s，
// 同号其他模型一并不可调度（2026-10-24 实案：单次 6020 即整号冻结 10 分钟）。
// 正确处置与 6021 一致：不罚号、不轮转，末端 503 透传原文。
func TestClassifyQueueWaitingIsUpstreamBusy(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"429 + 6020 信封", http.StatusTooManyRequests, `{"code":6020,"msg":"queue.waiting.title","requestId":"a1"}`, ErrUpstreamBusy},
		{"429 + 6020 嵌套信封", http.StatusTooManyRequests, `{"error":{"data":{"code":6020,"msg":"queue.waiting.title"}}}`, ErrUpstreamBusy},
		{"429 + 6020 queue waiting 文案", http.StatusTooManyRequests, `{"code":6020,"msg":"queue waiting"}`, ErrUpstreamBusy},
		{"500 + 6020 也认（防上游换状态码）", http.StatusInternalServerError, `{"code":6020,"msg":"queue.waiting.title"}`, ErrUpstreamBusy},
		{"6021 仍然认", http.StatusTooManyRequests, `{"code":6021,"msg":"queue.full.title"}`, ErrUpstreamBusy},
		// 防过宽：requestId 里恰好含 "6020" 不是业务码（hasBusinessCode 只认 code 键）。
		{"requestId 含 6020 不误伤", http.StatusTooManyRequests, `{"requestId":"6020","msg":"rate limit"}`, ErrSoftRate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("Classify(%d, %s)=%v want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestFrameKindQueueWaitingIsUpstreamBusy 流式与非流式口径必须一致：
// 流式 error 帧把业务码搬到 error.code，6020 同样要判成 ErrUpstreamBusy。
func TestFrameKindQueueWaitingIsUpstreamBusy(t *testing.T) {
	body := `{"code":6020,"msg":"queue.waiting.title"}`
	streamFrame := `{"error":{"code":6020,"message":"queue.waiting.title"}}`
	if got := Classify(http.StatusTooManyRequests, body); got != ErrUpstreamBusy {
		t.Fatalf("非流式 Classify=%v want ErrUpstreamBusy", got)
	}
	if got := FrameKind(streamFrame); got != ErrUpstreamBusy {
		t.Fatalf("流式 FrameKind=%v want ErrUpstreamBusy", got)
	}
}
