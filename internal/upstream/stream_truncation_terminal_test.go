package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamFinishReasons 收集一条 SSE 输出里所有非空 finish_reason（按出现顺序）。
func streamFinishReasons(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(ln, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("帧不是合法 JSON: %q (%v)", payload, err)
		}
		chs, _ := obj["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		ch, _ := chs[0].(map[string]any)
		fr, _ := ch["finish_reason"].(string)
		if fr != "" {
			out = append(out, fr)
		}
	}
	return out
}

// TestStreamInterruptedEmitsTruncationTerminal 上游断流（只给了一个正文帧就 EOF，
// 既无 data: [DONE] 也无 finish_reason 帧）时，网关必须补一帧截断终态
// finish_reason=length，而不是只补一个 data: [DONE] 假装正常收尾。
//
// 根因：只补 [DONE] 时客户端以为这一轮完整结束 → 半截内容被当完整答案落库，
// 静默丢内容且无感知（修复参考 reporter fork 提交 84a48d9）。
func TestStreamInterruptedEmitsTruncationTerminal(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"半截内容\"}}]}\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatalf("断流不应报错: %v", err)
	}
	body := rec.Body.String()
	frs := streamFinishReasons(t, body)
	n := 0
	for _, fr := range frs {
		if fr == TruncationFinishReason {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("截断终态 finish_reason=%s 应恰好出现 1 次，实际 %v（body=%q）", TruncationFinishReason, frs, body)
	}
	// 终态必须是最后一帧 finish，且 [DONE] 仍然只有一个。
	if len(frs) == 0 || frs[len(frs)-1] != TruncationFinishReason {
		t.Errorf("截断终态应在最后，实际 finish 序列 %v", frs)
	}
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", got, body)
	}
	// 正文不能丢。
	if !strings.Contains(body, "半截内容") {
		t.Errorf("正文丢失: %q", body)
	}
}

// TestStreamInterruptedWithTwoFramesEmitsTruncationTerminal 两个有效帧后断流，
// 同样必须补截断终态（validFrames>0 是前提，与帧数无关）。
func TestStreamInterruptedWithTwoFramesEmitsTruncationTerminal(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"}}]}\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatalf("断流不应报错: %v", err)
	}
	frs := streamFinishReasons(t, rec.Body.String())
	if len(frs) != 1 || frs[0] != TruncationFinishReason {
		t.Fatalf("finish 序列应只有一个 %s，实际 %v", TruncationFinishReason, frs)
	}
}

// TestStreamNormalDoneNoExtraTerminal 正常结束的流（带 finish_reason=stop 且有
// [DONE]）不得出现多余的截断终态。
func TestStreamNormalDoneNoExtraTerminal(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	frs := streamFinishReasons(t, body)
	if len(frs) != 1 || frs[0] != "stop" {
		t.Fatalf("finish 序列应只有一个 stop，实际 %v（body=%q）", frs, body)
	}
	if strings.Contains(body, TruncationFinishReason) {
		t.Errorf("正常流不应出现截断终态 %s: %q", TruncationFinishReason, body)
	}
}

// TestStreamFinishFrameWithoutDoneNoExtraTerminal 上游发了 finish_reason 帧但漏发
// [DONE]：本轮**已有终态**，不得再补一帧截断终态（否则一轮两个 finish 帧）。
func TestStreamFinishFrameWithoutDoneNoExtraTerminal(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	frs := streamFinishReasons(t, rec.Body.String())
	if len(frs) != 1 || frs[0] != "stop" {
		t.Fatalf("finish 序列应只有一个 stop，实际 %v", frs)
	}
}

// TestStreamZeroValidFramesNoFakeTerminal 一个有效帧都没收到（空流）时不得补截断
// 终态：空流走 empty upstream stream 错误帧（HTTP 502 语义），再造一个 finish
// 帧就是假终态。
func TestStreamZeroValidFramesNoFakeTerminal(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"完全空流", ""},
		{"只有注释行", ": comment\n\n"},
		{"只有 DONE", "data: [DONE]\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := Stream(rec, strings.NewReader(c.raw)); err == nil {
				t.Fatalf("空流应报错")
			}
			body := rec.Body.String()
			for _, fr := range streamFinishReasons(t, body) {
				if fr != "" {
					t.Errorf("空流不应有 finish 帧（假终态），实际 %q: %q", fr, body)
				}
			}
			if strings.Contains(body, TruncationFinishReason) {
				t.Errorf("空流不应出现截断终态 %s: %q", TruncationFinishReason, body)
			}
		})
	}
}

// TestStreamTruncatedTerminalReasonConstant 截断终态的取值必须落在协议词表内，
// 且三条适配器都能把它映射成「截断」而不是「正常完成」：
//   - chat：finish_reason=length；
//   - responses：response.incomplete + incomplete_details.reason=max_output_tokens；
//   - messages：stop_reason=max_tokens。
func TestStreamTruncatedTerminalReasonConstant(t *testing.T) {
	if TruncationFinishReason != "length" {
		t.Fatalf("TruncationFinishReason=%q，协议词表里没有截断语义（应为 length）", TruncationFinishReason)
	}
}
