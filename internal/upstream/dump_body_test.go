package upstream

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// captureLog 把 log 输出重定向到 buffer，返回恢复函数。
func captureLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	old := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	return buf, func() {
		log.SetOutput(old)
		log.SetFlags(oldFlags)
	}
}

// TestDumpBodyPrintsInboundAndPrepared WB2A_DUMP_BODY 开关必须同时打印
// 入站 body 与出站 body —— 出站 body 才是上游真正看到的形态。
func TestDumpBodyPrintsInboundAndPrepared(t *testing.T) {
	t.Setenv("WB2A_DUMP_BODY", "1")
	buf, restore := captureLog(t)
	defer restore()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, _, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	rc.Close()
	out := buf.String()
	if !strings.Contains(out, "[dbg-body] inbound") {
		t.Fatalf("缺少入站 body dump:\n%s", out)
	}
	if !strings.Contains(out, "[dbg-body] prepared") {
		t.Fatalf("缺少出站 body dump:\n%s", out)
	}
}

// TestDumpBodyPrintsOutboundHeaders 出站头必须整体打印（Authorization 只打长度）。
func TestDumpBodyPrintsOutboundHeaders(t *testing.T) {
	t.Setenv("WB2A_DUMP_BODY", "1")
	buf, restore := captureLog(t)
	defer restore()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, _, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	rc.Close()
	out := buf.String()
	if !strings.Contains(out, "[dbg-headers]") {
		t.Fatalf("缺少出站头 dump:\n%s", out)
	}
	// 敏感头不得明文。
	if strings.Contains(out, "Bearer at") {
		t.Fatalf("Authorization 不得明文打印:\n%s", out)
	}
	if !strings.Contains(out, "redacted len=") {
		t.Fatalf("Authorization 应打长度而不是值:\n%s", out)
	}
	// 关键头必须可见（渠道校验就是看这些）。
	for _, h := range []string{"User-Agent:", "X-Conversation-Id:", "Origin:", "Referer:"} {
		if !strings.Contains(out, h) {
			t.Fatalf("出站头 dump 缺少 %s:\n%s", h, out)
		}
	}
}

// TestDumpBodyOffByDefault 没设开关时不得打印任何东西。
func TestDumpBodyOffByDefault(t *testing.T) {
	os.Unsetenv("WB2A_DUMP_BODY")
	buf, restore := captureLog(t)
	defer restore()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, _, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	rc.Close()
	if out := buf.String(); strings.Contains(out, "dbg-") {
		t.Fatalf("未开开关却打印了调试信息:\n%s", out)
	}
}

// TestDumpTruncate 长 body 必须截断并标注（否则日志被冲垮、提示词大面积留盘）。
func TestDumpTruncate(t *testing.T) {
	short := []byte("abc")
	if got := dumpTruncate(short); got != "abc" {
		t.Fatalf("短 body 不该截断: %q", got)
	}
	long := []byte(strings.Repeat("x", dumpBodyLimit+500))
	got := dumpTruncate(long)
	if len(got) >= len(long) {
		t.Fatalf("长 body 未截断: %d >= %d", len(got), len(long))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("截断处应有标注: %q", got[:80])
	}
	if !strings.Contains(got, "total 4500 bytes") {
		t.Fatalf("截断处应说明原始长度: %q", got[:120])
	}
}
