package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestChatSyncClientCancelIsNotUpstreamParse 非流式请求的**调用方先断开**时，
// 不能记成 502 upstream_parse——那会把「别人掐断」报成「上游故障」。
//
// 线上现场（上游 issue #117 的邻案）：前置 CDN 回源超时（EdgeOne 默认 15s）
// 掐断连接，网关侧看到的就是 ctx canceled；此前一律记 502，一天十几次，
// 全是 CDN 掐的，网关本身毫发无损。
//
// 契约：
//   - ctx 已取消 → 记 499 + outcome=interrupted，**不写响应体**（连接已关）；
//   - ctx 未取消（上游真断）→ 仍走 502 upstream_parse。
func TestChatSyncClientCancelIsNotUpstreamParse(t *testing.T) {
	// 假上游：返回 200 + text/event-stream，但**正文一直不给**（模拟上游还在慢慢生成）。
	// 客户端随后断开 ⇒ 读上游的那次 Read 失败（ctx canceled）⇒ Aggregate 报错，
	// 此时 r.Context().Err() != nil ⇒ 应判「调用方断开」而不是「上游解析失败」。
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &blockingBody{ctx: r.Context()},
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"glm-5.2","messages":[]}`))).WithContext(ctx)
	rec := httptest.NewRecorder()
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel() // 调用方走了
	}()

	h.ServeHTTP(rec, req)

	// 连接已关：不写响应体（写了也发不出去）。
	if rec.Body.Len() != 0 {
		t.Errorf("调用方断开后不应写响应体, got %q", rec.Body.String())
	}
	// 账号不应被当成上游故障惩罚。
	st, _ := p.Status("u1")
	if st.ErrTotal != 0 {
		t.Errorf("调用方断开不得计入账号错误数: %+v", st)
	}
	if st.Cooling || st.Disabled {
		t.Errorf("调用方断开不得冷却/禁用账号: %+v", st)
	}
}

// TestChatSyncUpstreamBrokenStill502 上游真断（ctx 没取消）时必须仍然是 502
// upstream_parse——499 只归「调用方先走」，不能把上游故障也吞成 499。
func TestChatSyncUpstreamBrokenStill502(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {broken\n\n")),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"glm-5.2","messages":[]}`)))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s (上游真断必须 502)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "upstream_parse") {
		t.Errorf("body=%s 应含 upstream_parse", rec.Body.String())
	}
	_ = time.Second
}

// blockingBody 模拟「上游 200 但正文迟迟不来」：Read 一直阻塞到 ctx 取消，
// 于是读端拿到的是 ctx 的取消错误（与真实链路里「客户端断开 ⇒ 上游读失败」同形）。
type blockingBody struct {
	ctx context.Context
}

func (b *blockingBody) Read(p []byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *blockingBody) Close() error { return nil }
