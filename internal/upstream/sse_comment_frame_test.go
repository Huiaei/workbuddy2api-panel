package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamCommentFrameKeepsBoundary 上游注释帧（心跳）后面不发空行、直接跟
// data 帧时，网关必须补回帧分隔空行，否则 data 帧被粘进注释块、被下游整块丢弃。
func TestStreamCommentFrameKeepsBoundary(t *testing.T) {
	raw := ": heartbeat\n" +
		"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"HI\"}}]}\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	// 注释帧必须是完整的 "\n\n" 帧（帧分隔已恢复）。
	if !strings.Contains(body, ": heartbeat\n\n") {
		t.Fatalf("注释帧后应补回帧分隔空行，实际输出: %q", body)
	}
	// data 帧不能被粘到注释行上。
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, ":") && strings.Contains(ln, "data: ") {
			t.Fatalf("data 帧被粘到注释行上: %q", ln)
		}
	}
	// 下游按 "\n\n" 切块后，每一块都必须以 "data: " 开头（注释块除外）。
	for _, block := range strings.Split(strings.TrimRight(body, "\n\n"), "\n\n") {
		if block == "" {
			continue
		}
		if !strings.HasPrefix(block, "data: ") && !strings.HasPrefix(block, ":") {
			t.Fatalf("块缺少 data: / 注释前缀，会被下游丢弃: %q", block)
		}
	}
	frames := viewSSE(t, body)
	if len(frames) != 1 || frames[0].content != "HI" {
		t.Fatalf("data 帧不应丢失: %#v\nbody=%q", frames, body)
	}
}
