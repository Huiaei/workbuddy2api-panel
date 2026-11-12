package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// captureUpstream 记录每次出站请求的 body，供「上游真收到的报文里是不是补位值」断言。
type captureUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
	auths  []string
	status int
	body   string
	stream bool
}

func (c *captureUpstream) rt() roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, raw)
		c.auths = append(c.auths, r.Header.Get("Authorization"))
		c.mu.Unlock()
		ct := "application/json"
		if c.stream {
			ct = "text/event-stream"
		}
		return &http.Response{
			StatusCode: c.status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(c.body)),
		}, nil
	}
}

func (c *captureUpstream) snapshot() ([][]byte, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.bodies...), append([]string(nil), c.auths...)
}

// newCaptureHandler 建一个带粘性存储的 handler + 能记录出站 body 的假上游。
func newCaptureHandler(t *testing.T, cap *captureUpstream) (*Handler, *pool.Pool, *bindStore) {
	t.Helper()
	up := &upstream.Client{
		HTTP:          &http.Client{Transport: cap.rt()},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
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
	return h, p, st
}

const emptyContentBody = `{"code":11151,"msg":"a message has empty content"}`

// TestChatEmptyContentFailFastKeepsBinding 11151 空 text 段：请求级错误 ⇒
// fail-fast 400 透传原文、零轮转、零惩罚、粘性保留。
//
// 变异验证：把 handler 的 ErrEmptyContent 分支撤掉 ⇒ 本用例失败（会走通用
// ErrClient 轮转，末端 503 no_healthy_account）。
func TestChatEmptyContentFailFastKeepsBinding(t *testing.T) {
	cap := &captureUpstream{status: 400, body: emptyContentBody}
	h, p, st := newCaptureHandler(t, cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "empty_content") {
		t.Fatalf("响应里应带 empty_content 错误码：%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "a message has empty content") {
		t.Fatalf("必须透传上游原文：%s", rec.Body)
	}
	_, auths := cap.snapshot()
	if len(auths) != 1 {
		t.Fatalf("上游调用次数=%d want 1（零轮转），auths=%v", len(auths), auths)
	}
	if auths[0] != "Bearer at-u1" {
		t.Fatalf("应只打粘性号 u1，实际 %s", auths[0])
	}
	s1, _ := p.Status("u1")
	if s1.Cooling || s1.Disabled || s1.ErrTotal != 0 || s1.BreakerFails != 0 {
		t.Errorf("11151 是请求级错误，不得罚账号：%+v", s1)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "u1" {
		t.Fatalf("粘性绑定必须保留，实际 %s ok=%v", uid, ok)
	}
	if st.delCnt != 0 {
		t.Errorf("粘性绑定不得被删除，delCnt=%d", st.delCnt)
	}
}

// TestChatEmptyContentRepairedOutbound 出站补位接线：客户端发空 text 段，
// 上游实际收到的 body 里必须是补位值（空格），而不是空串——这才是让请求能过的
// 那层修复；上面的 fail-fast 只是补位漏网时的兜底。
func TestChatEmptyContentRepairedOutbound(t *testing.T) {
	cap := &captureUpstream{status: 200, body: sseOK, stream: true}
	h, _, _ := newCaptureHandler(t, cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":""}]}],"metadata":{"conversation_id":"conv-1"}}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200)", rec.Code, rec.Body)
	}
	bodies, _ := cap.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("上游调用次数=%d want 1", len(bodies))
	}
	var obj map[string]any
	if err := json.Unmarshal(bodies[0], &obj); err != nil {
		t.Fatalf("出站 body 不是合法 JSON: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("出站 body 缺 messages：%s", bodies[0])
	}
	// 网关会在最前面插兜底 system 消息、并给用户消息补若干字段，所以不能假定下标 0，
	// 按「哪条消息的 content 是 parts 数组」去找。
	found := false
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok || len(parts) == 0 {
			continue
		}
		part, ok := parts[0].(map[string]any)
		if !ok || part["type"] != "text" {
			continue
		}
		found = true
		if got := part["text"]; got != " " {
			t.Fatalf("上游收到的 text=%q want 补位空格（出站补位未生效）", got)
		}
	}
	if !found {
		t.Fatalf("出站 body 里没找到 parts 形态的消息：%s", bodies[0])
	}
}
