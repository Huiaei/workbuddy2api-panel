package server

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游给了 tool_calls 分片但从不带 function.name：Responses 适配器拼不出合法
// function_call item，只能丢——丢弃本身是对的（无名调用客户端无法执行），但此前
// 一条日志都没有。客户端表现就是「模型说完一句就停」，而网关侧查不出任何东西。
//
// 参考 reporter fork 提交 44bc059（上游原提交带的测试依赖 reporter 独有的
// fakeContinueUpstream 助手，本仓库没有该助手，故此处按本仓库的
// responsesWriter 直驱写法重写）。
func TestResponsesLogsNamelessToolCallDrop(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	rec := httptest.NewRecorder()
	rw := &responsesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"content":"先看完整表单。"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/x\"}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()

	got := buf.String()
	if !strings.Contains(got, "上游给了工具调用但缺函数名") {
		t.Fatalf("缺函数名时应有 WARN，实际日志:\n%s", got)
	}
	// 正文必须照常下发——WARN 只是补日志，不能影响内容。
	if !strings.Contains(rec.Body.String(), "先看完整表单。") {
		t.Fatalf("正文应照常下发:\n%s", rec.Body.String())
	}
}
