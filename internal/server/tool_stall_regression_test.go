package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 「零 tool_calls 停摆」（issue #149）的敌对回归测试。
//
// 这组测试覆盖首轮实现里的四个盲区（每一项都由对抗复审实测发现）：
//
//	A. 粘性绑定让「换号」空转：停摆分支只 releaseHeld()、不解绑粘性 → 下一轮
//	   PickByUIDForModel(stickyUID) 无视 tried 又选中同一个停摆号 → 同一个号被问
//	   两遍，第二个号从来没被调用（默认 session_sticky.enabled=true → 默认部署必现）。
//	B. 停摆把 tried 打满 → 上游明明回了正文却被丢弃 → 客户端收到 503
//	   no_healthy_account（单账号池必现）。
//	C. 判定条件「本请求声明了 tools」不充分：真实 agentic 客户端每轮都重发同一份
//	   tools 声明 → 正常收尾轮（最终答案 + stop + 零 tool_calls）被误判成停摆 →
//	   每轮多花一次调用 + 一次计费。
//	D. 流式与非流式判定相反：非流式先跑标记修复再判定（不判停摆），流式观测原始
//	   帧（判停摆）→ 同一形态两种口径。
//
// 另外覆盖截断排除（传输层 EOF 不算上游行为缺陷）。

// TestToolStallRetryActuallySwitchesAccountWithStickySession 回归 A：
// 开着粘性（默认部署形态）时，停摆重试必须**真的换到另一个号**，而不是把同一个
// 停摆号再问一遍。
func TestToolStallRetryActuallySwitchesAccountWithStickySession(t *testing.T) {
	var perAcct = map[string]*int32{}
	var u1Calls, u2Calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		switch authz {
		case "Bearer at1":
			atomic.AddInt32(&u1Calls, 1)
			return 200, stallSSE("我这就去查天气"), true
		case "Bearer at2":
			atomic.AddInt32(&u2Calls, 1)
			return 200, toolCallSSE("get_weather"), true
		}
		atomic.AddInt32(&u1Calls, 1)
		return 200, stallSSE("我这就去查天气"), true
	})
	_ = perAcct
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	// 粘性路由：会话预绑定到 u1（模拟上一轮已建立的粘性绑定）。
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Available: p.AvailableUIDs,
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, ToolStallRetry: 1})
	sess.Bind("conv-stall", "u1")

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"查一下今天的天气"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],` +
		`"metadata":{"conversation_id":"conv-stall"}}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 关键断言：u1 只能被问**一次**（停摆后必须换号），u2 必须被问一次。
	if got := atomic.LoadInt32(&u1Calls); got != 1 {
		t.Errorf("u1 upstream calls=%d want 1 (stall retry must actually switch account; sticky binding must be released)", got)
	}
	if got := atomic.LoadInt32(&u2Calls); got != 1 {
		t.Errorf("u2 upstream calls=%d want 1 (the second account must be tried)", got)
	}
}

// TestToolStallSingleAccountStillReturnsContent 回归 B：单账号池里停摆时，
// **上游回了正文**就必须把正文返回（200），不能掉进末端的 503 no_healthy_account。
// 「模型答了但没调工具」和「服务端没有可用账号」是两件事。
func TestToolStallSingleAccountStillReturnsContent(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSE("我这就去查天气"), true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(agenticBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (a stall must not turn a model answer into 503 no_healthy_account)", rec.Code, rec.Body)
	}
	if !contains(rec.Body.String(), "我这就去查天气") {
		t.Errorf("body=%s want the upstream text preserved", rec.Body)
	}
}

// TestToolStallFinalAnswerTurnWithToolsIsNotStall 回归 C：多轮会话的**正常收尾轮**。
//
// 真实 agentic 客户端（DSH / Codex / Cline）每轮都重发同一份 tools 声明。历史里
// 已经有工具结果 → 工具**跑过了** → 本轮的正文是最终答案 → 不是停摆，绝不能重试。
func TestToolStallFinalAnswerTurnWithToolsIsNotStall(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSE("好的，这是最终答案"), true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 1})

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"user","content":"查天气"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"晴 25 度"},` +
		`{"role":"user","content":"再详细点"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls=%d want 1 (a normal final-answer turn must not trigger a stall retry)", got)
	}
}

// TestToolStallToolCallWithoutResultIsStillStall 回归 C 的另一半：历史里有
// tool_calls 但**还没有工具结果** → 上一轮刚要调工具、本轮正文就停住了 → 正是
// 停摆现场，必须判停摆（不能因为「历史里有工具痕迹」就放过）。
func TestToolStallToolCallWithoutResultIsStillStall(t *testing.T) {
	tools := map[string]bool{"get_weather": true}
	// 历史里有 assistant.tool_calls，但没有 role=tool 的结果。
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"查天气"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"user","content":"继续"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	if upstream.HistoryHasToolResult(body) {
		t.Fatal("history has no tool result — must report false")
	}
	// 历史里有 role=tool 的结果 → true。
	body2 := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"c1"}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"晴"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	if !upstream.HistoryHasToolResult(body2) {
		t.Fatal("history has a tool result — must report true")
	}
	_ = tools
}

// TestToolStallTruncatedStreamIsNotStall 截断排除：上游没发 [DONE] 就 EOF 收尾
// （传输层截断）时，本轮的零 tool_calls 是「工具调用帧还没到」，不是「模型不调
// 工具」→ 不判停摆（否则一次网络抖动会被记成上游行为缺陷）。
func TestToolStallTruncatedStreamIsNotStall(t *testing.T) {
	truncated := map[string]any{
		"_truncated": true,
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "半句话"},
			"finish_reason": "stop",
		}},
	}
	if got := upstream.DetectToolStall(truncated, map[string]bool{"t": true}, false, false); got.Suspect {
		t.Errorf("truncated stream must not be suspect: %+v", got)
	}
	// 不带截断标记时照常判定。
	normal := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "半句话"},
			"finish_reason": "stop",
		}},
	}
	if got := upstream.DetectToolStall(normal, map[string]bool{"t": true}, false, false); !got.Suspect {
		t.Errorf("non-truncated zero-tool_call stop must be suspect: %+v", got)
	}
}

// TestStreamToolStallTruncatedNotSuspect 流式侧的截断排除：探测器收到
// MarkTruncated(true) 后不得判停摆。
func TestStreamToolStallTruncatedNotSuspect(t *testing.T) {
	d := upstream.NewStreamToolStallDetector(map[string]bool{"t": true}, false, false)
	d.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": "半句话"},
	}}})
	d.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{}, "finish_reason": "stop",
	}}})
	d.MarkTruncated(true)
	if got := d.Info(); got.Suspect {
		t.Errorf("truncated stream must not be suspect: %+v", got)
	}
	// 反之：显式 [DONE]（truncated=false）不改变判定。
	d2 := upstream.NewStreamToolStallDetector(map[string]bool{"t": true}, false, false)
	d2.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": "半句话"},
	}}})
	d2.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{}, "finish_reason": "stop",
	}}})
	d2.MarkTruncated(false)
	if got := d2.Info(); !got.Suspect {
		t.Errorf("complete stream with zero tool_calls must be suspect: %+v", got)
	}
}

// TestStreamToolStallRepairedNotSuspect 回归 D：流式路径上标记修复器还原出过工具
// 调用 → 上游**确实调了工具**（只是吐成了文本标记）→ 不判停摆。这让流式与非流式
// 对同一形态给出相同结论。
func TestStreamToolStallRepairedNotSuspect(t *testing.T) {
	d := upstream.NewStreamToolStallDetector(map[string]bool{"t": true}, false, false)
	d.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": "原文标记"},
	}}})
	d.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{}, "finish_reason": "stop",
	}}})
	d.MarkRepaired(1)
	if got := d.Info(); got.Suspect {
		t.Errorf("repaired stream (upstream did call the tool) must not be suspect: %+v", got)
	}
}

// TestStreamToolStallHistoryHasResultNotSuspect 流式侧的「历史已有工具结果」排除。
func TestStreamToolStallHistoryHasResultNotSuspect(t *testing.T) {
	d := upstream.NewStreamToolStallDetector(map[string]bool{"t": true}, true, false)
	d.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": "最终答案"},
	}}})
	d.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{}, "finish_reason": "stop",
	}}})
	if got := d.Info(); got.Suspect {
		t.Errorf("history with tool result must not be suspect: %+v", got)
	}
}

// contains 是 strings.Contains 的本地小别名（避免为一个断言再 import strings）。
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestToolStallRetryDoesNotConsumeErrorRecoveryBudget 回归 F：停摆重试**不消耗**
// MaxRotate 的换号预算。实测（对抗复审）：跳 1、2 停摆，跳 3 真 429 → 客户端直接
// 吃 429，没人再救——因为停摆把 3 次换号机会全吃光了。
// 期望：停摆重试之后仍保留错误恢复能力，429 仍能被换号救回来（最终 200）。
func TestToolStallRetryDoesNotConsumeErrorRecoveryBudget(t *testing.T) {
	var u1Calls, u2Calls, u3Calls, u4Calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		switch authz {
		case "Bearer at1":
			atomic.AddInt32(&u1Calls, 1)
			return 200, stallSSE("我这就去查天气"), true
		case "Bearer at2":
			atomic.AddInt32(&u2Calls, 1)
			return 200, stallSSE("我这就去查天气"), true
		case "Bearer at3":
			atomic.AddInt32(&u3Calls, 1)
			return 429, `{"code":429,"msg":"rate limited"}`, false
		case "Bearer at4":
			atomic.AddInt32(&u4Calls, 1)
			return 200, toolCallSSE("get_weather"), true
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u4", AccessToken: "at4", ExpiresAt: 9999999999},
	)
	// MaxRotate=2：正常情况下只有 2 次**错误恢复**换号机会。停摆重试若吃预算，
	// u3（429）之后就没有换号机会了，客户端直接吃 429。
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 2, MaxRotate: 2, SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(agenticBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (a 429 on the third account must still be recoverable by rotation)", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&u3Calls); got == 0 {
		t.Errorf("u3 was never tried — stall retries consumed the error-recovery budget")
	}
	if got := atomic.LoadInt32(&u4Calls); got == 0 {
		t.Errorf("u4 was never tried — the 429 on u3 was not recovered by rotation")
	}
}
