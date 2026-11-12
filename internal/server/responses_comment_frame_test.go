package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResponsesStreamPassesThroughCommentFrames SSE 注释帧（": heartbeat"）必须原样
// 透传给客户端。
//
// 背景：上游 workbuddy.cn 的 SSE 会在 data 帧之间插注释帧保活。注释帧的协议语义
// 就是 no-op，但它的**唯一作用**是让连接「有字节流动」——网关若把它吞掉，客户端
// 看不到任何字节就会按 stream idle 超时掐线（实测日志里 14 次 interrupted 与 8 次
// 「段无 finish 结束」在时间上重合）。
//
// 注释帧还有一个连带效应：responses 侧按 "\n\n" 切帧，若注释帧与后续 data 帧被
// 粘成一个块，整块没有 "data: " 前缀 → 整帧被丢弃（客户端表现为工具调用首帧消失）。
// 所以这里既验证「透传」，也验证「注释帧后紧跟的 data 帧仍然被正常解析」。
func TestResponsesStreamPassesThroughCommentFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		": heartbeat\n\n",
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
		": keep-alive\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	if !strings.Contains(body, ": heartbeat") {
		t.Fatalf("注释帧未透传: %s", body)
	}
	if !strings.Contains(body, ": keep-alive") {
		t.Fatalf("注释帧未透传: %s", body)
	}
	// 注释帧不得混进事件流（不占 sequence_number，也不得被当成事件解析）。
	types := sseTypes(body)
	for _, ty := range types {
		if strings.HasPrefix(ty, ":") {
			t.Fatalf("注释帧被当成事件: %v", types)
		}
	}
	// 注释帧之后的 data 帧必须仍然被解析（不能被粘成一个块丢弃）。
	if !strings.Contains(body, `"delta":"hi"`) {
		t.Fatalf("注释帧之后的 data 帧丢失: %s", body)
	}
}

// TestResponsesCompletedOutputNeverNull completed 事件的 output 不能是 null。
//
// Go 的 nil 切片序列化成 JSON null，而 Responses 客户端（CodeBuddy/Codex）拿到
// completed 后会对 output 做 .map —— null 直接崩：
// "Cannot read properties of null (reading 'map')"。
// 空输出是合法响应（模型只思考不输出、或所有增量被上游吞掉），必须落成 []。
func TestResponsesCompletedOutputNeverNull(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		// 只带 id/model、没有 choices 的一帧：建立 response 但不产生任何 output item。
		`data: {"id":"chatcmpl-1","model":"m"}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	if strings.Contains(body, `"output":null`) {
		t.Fatalf("completed 的 output 为 null（客户端会对它 .map 崩溃）: %s", body)
	}
	if !strings.Contains(body, `"output":[]`) {
		t.Fatalf("completed 的 output 未落成空数组: %s", body)
	}
}

// TestResponsesCompletedOutputIsArray 有输出时 output 仍是数组（不被兜底逻辑改坏）。
func TestResponsesCompletedOutputIsArray(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	if strings.Contains(body, `"output":null`) {
		t.Fatalf("output 为 null: %s", body)
	}
	// output 必须是 JSON 数组（不是对象/字符串）。
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
				continue
			}
			resp, ok := m["response"].(map[string]any)
			if !ok {
				continue
			}
			out, ok := resp["output"]
			if !ok {
				continue
			}
			if _, isArr := out.([]any); !isArr {
				t.Fatalf("output 不是数组: %T body=%s", out, body)
			}
		}
	}
}

// TestMessagesStreamPassesThroughCommentFrames messages（Anthropic）侧同样必须
// 透传 SSE 注释帧：吞掉 = 客户端看不到字节流动 → 按 stream idle 超时掐线。
func TestMessagesStreamPassesThroughCommentFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &messagesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		": heartbeat\n\n",
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	if !strings.Contains(body, ": heartbeat") {
		t.Fatalf("注释帧未透传: %s", body)
	}
	// messages 侧是 Anthropic 事件协议：正文落在 content_block_delta 的 text 字段。
	if !strings.Contains(body, `"text":"hi"`) {
		t.Fatalf("注释帧之后的 data 帧丢失: %s", body)
	}
	if !strings.Contains(body, `"type":"content_block_delta"`) {
		t.Fatalf("注释帧之后的 data 帧丢失: %s", body)
	}
}
