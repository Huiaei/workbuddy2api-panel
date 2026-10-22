package upstream

import (
	"net/http"
	"testing"
)

// TestClassifyModelBusy 14003 必须归 ErrModelBusy，且**优先于**通用 429 兜底——
// 否则会按 ErrSoftRate 逐号 600s 软冷却，两三个号一叠就是整分钟黑障（issue #46）。
func TestClassifyModelBusy(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"400 数字码", http.StatusBadRequest, `{"code":14003,"msg":"model busy"}`},
		{"429 数字码（关键：不得落软限流）", http.StatusTooManyRequests, `{"code":14003,"msg":"model is busy, please retry"}`},
		{"字符串码", http.StatusBadRequest, `{"code":"14003","msg":"busy"}`},
		{"带空格", http.StatusOK, `{"error":{"code": 14003, "msg":"busy"}}`},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != ErrModelBusy {
			t.Fatalf("%s: Classify=%v want ErrModelBusy", c.name, got)
		}
	}
	// 负例：普通 429 仍是软限流；11102 仍是确定性模型不可用。
	if got := Classify(http.StatusTooManyRequests, `{"msg":"too many requests"}`); got != ErrSoftRate {
		t.Fatalf("普通 429 应为 ErrSoftRate，got=%v", got)
	}
	if got := Classify(http.StatusBadRequest, `{"code":11102,"msg":"not found"}`); got != ErrModelBlocked {
		t.Fatalf("11102 应为 ErrModelBlocked，got=%v", got)
	}
}

// TestModelBusyReasonFor reason 汇总上游原文并截断（面板表格要能放下）。
func TestModelBusyReasonFor(t *testing.T) {
	got := ModelBusyReasonFor(`{"code":14003,"msg":"model is busy"}`)
	if !regexpHasPrefix(got, "14003") || !containsStr(got, "model is busy") {
		t.Fatalf("reason=%q 应带 14003 前缀与上游原文", got)
	}
	long := ModelBusyReasonFor(`{"code":14003,"msg":"` + repeatStr("x", 400) + `"}`)
	if n := len([]rune(long)); n > 200 {
		t.Fatalf("reason 应截断到 ~160 字符，got len=%d", n)
	}
}

func regexpHasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
func repeatStr(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
