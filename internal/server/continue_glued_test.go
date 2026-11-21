package server

import (
	"io"
	"strings"
	"testing"
)

// continueReader：上游把多个 chunk 粘在一个 data 行里（issue #197）时拆成独立帧，正文照常
// 记账；粘在第一个位置的 error 帧被识别成错误帧（不记正文、不触发续写）。
//
// 与 reporter 的差别（裁剪项）：reporter 这条用例的第二段断言「粘在首位的 error 帧经
// StreamHint 仍触发首帧否决（upstream.HeadVetoError）」——首帧否决是本仓库没有的特性，
// 所以这里退到续写层的可见结果：errSeen 置位、正文不夹带错误帧。
func TestContinueReaderSplitsGluedFrames(t *testing.T) {
	frame := func(txt string) string {
		return `{"id":"a","choices":[{"index":0,"delta":{"content":"` + txt + `"}}]}`
	}
	open := func(stream string) *continueReader {
		up, _ := fakeContinueUpstream(t, func(int) (int, string) { return 200, "" })
		return newTestContinueReader(t, up, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
			io.NopCloser(strings.NewReader(stream)))
	}

	cont := open("data: " + frame("x") + " " + frame("y") + "\n\ndata: [DONE]\n\n")
	out, _ := io.ReadAll(cont)
	if cont.text.String() != "xy" || strings.Count(string(out), "data: {") != 2 {
		t.Fatalf("粘帧应拆成两帧且正文都记账: text=%q\n%s", cont.text.String(), out)
	}

	errFrame := `{"error":{"message":"busy","code":"6021"}}`
	cont = open("data: " + errFrame + " " + frame("x") + "\n\ndata: [DONE]\n\n")
	out, _ = io.ReadAll(cont)
	if !cont.errSeen {
		t.Fatalf("粘在首位的 error 帧应被识别成错误帧:\n%s", out)
	}
	if cont.text.String() != "x" {
		t.Fatalf("error 帧不应混进正文记账: text=%q", cont.text.String())
	}
}
