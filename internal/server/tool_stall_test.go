package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 零 tool_calls 停摆（issue #149）端到端测试。
//
// 形态：请求声明了 tools，模型却只回正文 + finish_reason=stop，工具一次没跑。
// 期望：换号重试一次（默认 ToolStallRetry=1），**且绝不罚账号**。

// stallSSE 构造一条「只回正文 + stop 收尾」的流式响应。
func stallSSE(text string) string {
	return "data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"" + text + "\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n\n"
}

// toolCallSSE 构造一条「正常调了工具」的流式响应。
func toolCallSSE(name string) string {
	return "data: {\"id\":\"chatcmpl-t\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"" + name + "\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-t\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n\n"
}

// agenticBody 一条声明了 tools 的请求体。
func agenticBody(model string) []byte {
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"查一下今天的天气"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather","description":"查天气","parameters":{"type":"object"}}}]}`)
}

// TestToolStallRetriesOnAnotherAccount 第一跳停摆 → 换号重试 → 第二跳正常：
// 期望 200、至少 2 次上游调用、**两个号都没被冷却/禁用**。
func TestToolStallRetriesOnAnotherAccount(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return 200, stallSSE("我这就去查天气"), true
		}
		return 200, toolCallSSE("get_weather"), true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(agenticBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (stall retry must produce a successful round)", rec.Code, rec.Body)
	}
	if calls < 2 {
		t.Fatalf("upstream calls=%d want >=2 (stall must trigger one retry)", calls)
	}
	// 关键断言：停摆**不罚账号**（上游行为缺陷，不是账号问题）。
	for _, a := range p.List() {
		if a.Disabled {
			t.Errorf("account %s disabled after stall — stall must not penalize accounts", a.UID)
		}
		if a.Cooling {
			t.Errorf("account %s cooling after stall — stall must not penalize accounts", a.UID)
		}
	}
}

// TestToolStallWithoutToolsIsNotSuspect 普通对话（没有 tools 声明）回「只回正文 + stop」
// 是**完全正常**的行为，绝不能被当成停摆、绝不能触发重试。
func TestToolStallWithoutToolsIsNotSuspect(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSE("今天天气不错"), true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 1})

	rec := httptest.NewRecorder()
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}]}`)
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1 (no tools declared → no stall → no retry)", calls)
	}
}

// TestToolStallRetryDisabled 当 ToolStallRetry=0 时不重试：只回正文的那一跳原样
// 透给客户端（客户端能看到模型的真实回答），上游只被调用一次。
func TestToolStallRetryDisabled(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSE("我这就去查天气"), true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 0})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(agenticBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1 (retry disabled)", calls)
	}
}

// TestToolStallExhaustedStillReturnsContent 所有号都停摆时：重试预算用尽后把
// **最后一次的正文原样**返回给客户端（200，不是错误）——模型确实答了，只是没调工具。
// 同时所有号都**没有**被冷却/禁用。
func TestToolStallExhaustedStillReturnsContent(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSE("我这就去查天气"), true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(agenticBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (stall exhausted must still return the model's text)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "我这就去查天气") {
		t.Errorf("body=%s want the upstream text preserved", rec.Body)
	}
	if calls < 2 {
		t.Fatalf("upstream calls=%d want >=2 (one retry)", calls)
	}
	for _, a := range p.List() {
		if a.Disabled {
			t.Errorf("account %s disabled after stall", a.UID)
		}
		if a.Cooling {
			t.Errorf("account %s cooling after stall", a.UID)
		}
	}
}

// TestDetectToolStallUnit 单元判定：只按「零 tool_calls + stop 收尾」判，
// **绝不按正文长度**判（报告方的教训：按长度判会把 glm-5.3 的停摆率算成 0%）。
func TestDetectToolStallUnit(t *testing.T) {
	tools := map[string]bool{"get_weather": true}

	// 正文很长（1200 字符）+ 零 tool_calls + stop → 仍然是停摆。
	long := strings.Repeat("x", 1200)
	body := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": long},
			"finish_reason": "stop",
		}},
	}
	if got := upstream.DetectToolStall(body, tools); !got.Suspect {
		t.Errorf("long text + zero tool_calls + stop must be suspect, got %+v", got)
	}
	if got := upstream.DetectToolStall(body, tools); got.ContentLen != 1200 {
		t.Errorf("ContentLen=%d want 1200", got.ContentLen)
	}

	// 有 tool_calls → 不是停摆。
	withCall := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}},
			"finish_reason": "tool_calls",
		}},
	}
	if got := upstream.DetectToolStall(withCall, tools); got.Suspect {
		t.Errorf("response with tool_calls must not be suspect: %+v", got)
	}

	// 没有声明 tools → 不是停摆（普通对话回正文是正常的）。
	if got := upstream.DetectToolStall(body, nil); got.Suspect {
		t.Errorf("no tools declared must not be suspect: %+v", got)
	}

	// finish_reason=length（截断）→ 不是停摆。
	trunc := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "半句"},
			"finish_reason": "length",
		}},
	}
	if got := upstream.DetectToolStall(trunc, tools); got.Suspect {
		t.Errorf("finish_reason=length must not be suspect: %+v", got)
	}

	// 短正文 + 零 tool_calls + stop → 仍然是停摆（长度不参与判定）。
	short := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "好"},
			"finish_reason": "stop",
		}},
	}
	if got := upstream.DetectToolStall(short, tools); !got.Suspect {
		t.Errorf("short text + zero tool_calls + stop must be suspect: %+v", got)
	}
}

// TestStreamToolStallDetector 流式探测器的口径与非流式一致。
func TestStreamToolStallDetector(t *testing.T) {
	tools := map[string]bool{"get_weather": true}

	// 只有正文 + stop → 停摆。
	d := upstream.NewStreamToolStallDetector(tools)
	d.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": "我这就去查"},
	}}})
	d.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{}, "finish_reason": "stop",
	}}})
	if got := d.Info(); !got.Suspect {
		t.Errorf("stream with text only + stop must be suspect: %+v", got)
	}

	// 有 tool_calls → 不停摆。
	d2 := upstream.NewStreamToolStallDetector(tools)
	d2.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1"}}},
	}}})
	d2.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{}, "finish_reason": "tool_calls",
	}}})
	if got := d2.Info(); got.Suspect {
		t.Errorf("stream with tool_calls must not be suspect: %+v", got)
	}

	// 没声明 tools → 不停摆。
	d3 := upstream.NewStreamToolStallDetector(nil)
	d3.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": "你好"},
	}}})
	d3.Observe(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{}, "finish_reason": "stop",
	}}})
	if got := d3.Info(); got.Suspect {
		t.Errorf("stream without declared tools must not be suspect: %+v", got)
	}
}

// TestToolStallHistoryToolCallsIsNotStall 多轮会话的**正常收尾**：本请求没有声明
// tools（只是历史里有 tool_calls），模型给出最终答案（stop + 零 tool_calls）。
// 这**不是**停摆，绝不能触发换号重试——否则一次正常回答变成两次上游调用。
func TestToolStallHistoryToolCallsIsNotStall(t *testing.T) {
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

	rec := httptest.NewRecorder()
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"user","content":"查天气"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"晴 25 度"},` +
		`{"role":"user","content":"再详细点"}]}`)
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1 (history tool_calls must not trigger a stall retry)", calls)
	}
}

// TestToolStallToolChoiceNoneIsNotStall 客户端用 tool_choice="none" **显式禁用**了
// 工具调用：模型只回正文 + stop 是**正确**行为，不是停摆，绝不能重试。
func TestToolStallToolChoiceNoneIsNotStall(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSE("今天天气不错"), true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 1})

	rec := httptest.NewRecorder()
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],` +
		`"tool_choice":"none"}`)
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1 (tool_choice=none must not trigger a stall retry)", calls)
	}
}

// TestToolStallHopIsStillBilled 停摆的那一跳**照样计费**：上游真的花了 token，
// 账本必须记到那个号头上。若漏记，账号余额被高估（选号继续往它身上派），
// 且积分燃尽熔断（issue #229）会漏计这一跳的消耗。
func TestToolStallHopIsStillBilled(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSE("我这就去查天气"), true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(agenticBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 两跳都该被记账：每跳 usage 是 prompt 10 / completion 5 / total 15。
	total := int64(0)
	for _, s := range p.List() {
		total += s.TokenUsage.TotalTokens
	}
	if total != 30 {
		t.Errorf("total tokens across accounts=%d want 30 (both hops must be billed)", total)
	}
}

// TestToolStallRecordedInRequestLog 停摆要能进请求日志（运维观测）：归档事件里
// 带 tool_stall=true 与当时的正文字数，运维才能统计停摆率与模型分布。
func TestToolStallRecordedInRequestLog(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSE("我这就去查天气"), true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rl := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: rl, ToolStallRetry: 0})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(agenticBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	events := rl.Snapshot().Recent
	if len(events) == 0 {
		t.Fatal("no request-log event recorded")
	}
	var found bool
	for _, e := range events {
		if e.ToolStall {
			found = true
			if e.ToolStallText != len("我这就去查天气") {
				t.Errorf("tool_stall_text=%d want %d", e.ToolStallText, len("我这就去查天气"))
			}
		}
	}
	if !found {
		t.Errorf("no event with tool_stall=true: %+v", events)
	}
}
