// continue_fallthrough_test.go 救援请求本身失败时，客户端拿到的仍是原始那一段。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// scriptedChatHandler 造一个单账号 handler，上游第 n 次调用返回 segs[n-1]（超出时重复最后一段）。
func scriptedChatHandler(t *testing.T, segs ...string) (*Handler, *[]string) {
	t.Helper()
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		if call > len(segs) {
			call = len(segs)
		}
		return 200, segs[call-1]
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	return NewHandler(Config{Pool: p, Upstream: up}), reqs
}

// TestNarrationRescueFailureKeepsOriginalOutput 救援是「锦上添花」：救援请求失败（500）
// 时，客户端必须原样收到第一段正文，不能吞掉、不能改写、也不能把一个失败的上游
// 响应变成错误响应（HTTP 头早就是 200 了）。
//
// 这是移植后的**关键回归点**：救援链挂在帧透传路径上，任何提前 return 或清空
// tail 的写法都会让「本来能用的那一段」跟着消失。
func TestNarrationRescueFailureKeepsOriginalOutput(t *testing.T) {
	lead := `data: {"id":"n1","choices":[{"index":0,"delta":{"content":"先看完整表单："}}]}` + "\n\n" +
		`data: {"id":"n1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268538,"completion_tokens":21,"total_tokens":268559}}` + "\n\n" +
		"data: [DONE]\n\n"
	h, reqs := scriptedChatHandler(t, lead, `data: {"error":{"message":"boom","code":"6021"}}`+"\n\ndata: [DONE]\n\n")
	body := `{"model":"deepseek-v4.1-flash","stream":true,"tools":[{"type":"function","function":{"name":"read_file"}}],` +
		`"messages":[{"role":"user","content":"看看这个表单"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"<form>…</form>"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	out := rec.Body.String()
	if len(*reqs) != 2 {
		t.Fatalf("应尝试一次救援（首段 + 救援段），实际上游调用 %d 次", len(*reqs))
	}
	if !strings.Contains(out, "先看完整表单：") {
		t.Fatalf("救援失败不得吞掉首段正文:\n%s", out)
	}
	if rec.Code != 200 {
		t.Fatalf("救援失败不应改变 HTTP 状态，got %d", rec.Code)
	}
	// 救援段的 error 帧按 reporter 语义原样透传给客户端（帧是上游真实响应，隐藏它等于
	// 伪造一次成功），客户端据此自行重试；网关侧只负责把首段正文留在原地。
	if !strings.Contains(out, `"error"`) {
		t.Fatalf("救援段的 error 帧应透传（客户端需要知道这一轮没走完）:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("流应以 [DONE] 收尾（不能让客户端挂到超时）:\n%s", out)
	}
}
