package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 停摆跳与「积分燃尽熔断」（issue #229）的口径对称性。
//
// 设计原则（见 internal/upstream/tool_stall.go 头部注释）：停摆是**上游行为缺陷**，
// 不是账号问题 → 命中后**绝不罚账号**。「不罚」在池层面包含两件事：
//  1. 不冷却 / 不禁用 / 不 NoteFailures / 不 NoteError（handler 的 applyErrorPolicy）；
//  2. **不喂燃尽熔断的计账**——把「模型这一轮没调工具」记成「这个号在烧积分」
//     会让熔断器误伤健康号（开启熔断的部署会中招）。
//
// 第 2 条在非流式路径有守卫（`if !stall.Suspect`），但流式路径曾经**无条件**
// NoteBurn → 同一形态（声明了 tools、零 tool_calls、stop 收尾）在流式下会被熔断、
// 在非流式下不会 → 熔断结果随请求模式漂移，统计与排障都失去意义。
//
// 本文件锁定这条对称性：两条路径下停摆跳都**不**喂燃尽熔断。

// streamBody 返回一条**显式声明 "stream": true** 的 agentic 请求体。
//
// 为什么必须显式声明：handler 用 peek.Stream 决定走流式分支还是聚合分支
// （internal/server/handler.go 的 peek 结构体 + `if peek.Stream`），
// 不带这个字段就走聚合路径 —— 于是「流式」用例其实测的是非流式（假绿）。
func streamBody(model string) []byte {
	return []byte(strings.Replace(string(agenticBody(model)), `{"model"`, `{"stream":true,"model"`, 1))
}

// stallSSECredit 构造一条「只回正文 + stop 收尾 + 带 credit 的 usage」的流式响应。
//
// 为什么需要 credit：流式路径的记账闸门是 validUsageCost —— credit 与
// total_tokens **都要**有效才记积分（见 handler.go 的 validUsageCost），
// 所以测燃尽熔断必须让 usage 里带上 credit。
func stallSSECredit(text string, credit float64) string {
	return "data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"" + text + "\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"credit\":" +
		strconv.FormatFloat(credit, 'f', -1, 64) + "}}\n\n" +
		"data: [DONE]\n\n"
}

// toolCallSSECredit 构造一条「正常调了工具 + 带 credit 的 usage」的流式响应。
func toolCallSSECredit(name string, credit float64) string {
	return "data: {\"id\":\"chatcmpl-t\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"" + name + "\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-t\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"credit\":" +
		strconv.FormatFloat(credit, 'f', -1, 64) + "}}\n\n" +
		"data: [DONE]\n\n"
}

// TestStallHopDoesNotTripBurnGuardInStream 流式停摆跳不得喂燃尽熔断。
//
// 阈值设为 0.5 积分、停摆跳的 credit 是 1 积分：若喂了熔断计账，spent=1 > 0.5
// → 账号立刻熔断。
func TestStallHopDoesNotTripBurnGuardInStream(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSECredit("我这就去查天气", 1), true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 阈值 0.5 积分 < 本跳 credit（1 积分）：若这一跳喂了熔断计账，账号立刻熔断。
	p.SetBurnGuard(0.5, time.Minute, time.Minute, time.Minute)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 0})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(streamBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	for _, s := range p.List() {
		if s.Cooling {
			t.Errorf("停摆跳不应把账号喂进燃尽熔断，但账号 %s 处于冷却（reason=%q）", s.UID, s.Reason)
		}
	}
}

// TestStallHopDoesNotTripBurnGuardInNonStream 非流式停摆跳同样不得喂燃尽熔断
// （与流式对称）。
//
// 注意两点：
//  1. 请求体必须带 "stream": false，否则 handler 会走流式分支（peek.Stream）。
//  2. 上游返回的**仍然是 SSE 帧流**（upstream.Aggregate 聚合的是 SSE），
//     「非流式」只是网关把聚合结果整体写回客户端。所以「非流式」的响应体
//     同样要用 SSE 帧构造，不能用普通 JSON。
func TestStallHopDoesNotTripBurnGuardInNonStream(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		// 上游侧仍是 SSE 帧流；网关聚合后整体写回（非流式）。
		return 200, stallSSECredit("我这就去查天气", 1), true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBurnGuard(0.5, time.Minute, time.Minute, time.Minute)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 0})

	rec := httptest.NewRecorder()
	body := []byte(strings.Replace(string(agenticBody("glm-5.2")), `{"model"`, `{"stream":false,"model"`, 1))
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	for _, s := range p.List() {
		if s.Cooling {
			t.Errorf("停摆跳不应把账号喂进燃尽熔断，但账号 %s 处于冷却（reason=%q）", s.UID, s.Reason)
		}
	}
}

// TestNormalHopStillTripsBurnGuard 反向对照：**正常**（真调了工具）的一跳
// 必须照常喂燃尽熔断——否则上面的两个用例会因为「熔断根本没生效」而假绿。
func TestNormalHopStillTripsBurnGuard(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, toolCallSSECredit("get_weather", 1), true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBurnGuard(0.5, time.Minute, time.Minute, time.Minute)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 0})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(streamBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var cooling bool
	for _, s := range p.List() {
		if s.Cooling {
			cooling = true
		}
	}
	if !cooling {
		t.Errorf("正常一跳消耗 1 积分（阈值 0.5）必须触发燃尽熔断，否则上面的用例是假绿")
	}
}

// TestStallHopStillBilledButNotBurned 停摆跳**照样记账**（余额要准），
// 只是**不参与**燃尽熔断计账。这两个动作必须同时成立。
func TestStallHopStillBilledButNotBurned(t *testing.T) {
	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 200, stallSSECredit("我这就去查天气", 1), true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBurnGuard(0.5, time.Minute, time.Minute, time.Minute)
	h := NewHandler(Config{Pool: p, Upstream: up, ToolStallRetry: 0})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(agenticBody("glm-5.2"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 记账：token 总量必须被记录（停摆跳也花了 token）。
	var total int64
	for _, s := range p.List() {
		total += s.TokenUsage.TotalTokens
	}
	if total != 15 {
		t.Errorf("停摆跳的 token 应记账，total=%d want 15", total)
	}
	// 熔断：不应触发。
	for _, s := range p.List() {
		if s.Cooling {
			t.Errorf("停摆跳不应喂燃尽熔断，但账号 %s 处于冷却", s.UID)
		}
	}
}

// 确认 pool.Pool 有 SetBurnGuard（编译期断言，避免接口漂移后静默失效）。
var _ = func() bool {
	var p *pool.Pool
	p.SetBurnGuard(1, time.Minute, time.Minute, time.Minute)
	return true
}
