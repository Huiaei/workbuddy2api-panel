package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件钉住**交叉审计第二轮 Q5.3** 抓到的契约缺陷：
//
// 「观察者只读不改对象」这条承诺**只对同步读取成立**——repairFrame 拿到的就是
// 同一个 map 引用，它会就地改写 delta.content 与 finish_reason。任何把帧对象
// **存下来**的观察者，事后读到的都是修复后的状态：
//
//	[observer] live[0] finish=""      contentLen=218 hasMarkup=true   ← 原始帧
//	[observer] kept[0] finish=""      contentLen=6   hasMarkup=false  ← 标记已被抹掉
//	[observer] kept[1] finish="tool_calls"                            ← stop 被改写
//
// 修法：交给观察者的是**深拷贝**，观察者拿到的永远是「上游原样」。

// markupFrameFixture 一条带 DSML 工具调用标记的流：正文帧带标记，末帧
// finish_reason=stop（repairFrame 会把它改写成 tool_calls）。
//
// 夹具用包内常量拼装（markupPipe / markupOpen / markupClose ...），不在源码里
// 写死全角分隔符字面量：分隔符是全角字符，字面量容易被编辑器/复制粘贴静默改坏。
func markupFrameFixture() string {
	param := "<" + markupPipe + "DSML" + markupPipe + " parameter name=\"city\" string=\"true\">杭州" + markupParamClose
	invoke := "<" + markupPipe + "DSML" + markupPipe + " invoke name=\"get_weather\">\n" + param + "\n" + markupInvokeClose
	block := markupOpen + "\n" + invoke + "\n" + markupClose
	// 用 json.Marshal 造帧：标记块里带**真实换行**，直接拼进 JSON 字符串会让
	// 整帧变成非法 JSON（解析失败 ⇒ 该帧不进观察者 ⇒ 测试假绿）。
	mk := func(content, finish string) string {
		raw, err := json.Marshal(map[string]any{
			"id":     "c1",
			"object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"role": "assistant", "content": content},
				"finish_reason": finish,
			}},
		})
		if err != nil {
			panic(err)
		}
		return "data: " + string(raw) + "\n\n"
	}
	return mk("好的"+block, "") + mk("", "stop") + "data: [DONE]\n\n"
}

// TestFrameObserverReceivesUnmodifiedFrame 观察者拿到的帧必须是上游原样。
func TestFrameObserverReceivesUnmodifiedFrame(t *testing.T) {
	repair := NewMarkupRepair(map[string]bool{"get_weather": true}, false)
	var kept []map[string]any
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(markupFrameFixture()), nil,
		WithMarkupRepair(repair),
		WithFrameObserver(func(obj map[string]any) {
			kept = append(kept, obj)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) == 0 {
		t.Fatal("观察者没有被调用")
	}
	// 事后读取保存下来的帧：内容必须仍是上游原样（标记还在、finish_reason 还是 stop）。
	first := kept[0]
	chs, _ := first["choices"].([]any)
	if len(chs) == 0 {
		t.Fatal("首帧缺少 choices")
	}
	c0, _ := chs[0].(map[string]any)
	d0, _ := c0["delta"].(map[string]any)
	txt, _ := d0["content"].(string)
	if !containsMarkupCall(txt) {
		t.Fatalf("观察者保存的帧内容被就地改写了（标记消失）: %q", txt)
	}
	last := kept[len(kept)-1]
	lchs, _ := last["choices"].([]any)
	lc, _ := lchs[0].(map[string]any)
	if fr, _ := lc["finish_reason"].(string); fr != "stop" {
		t.Fatalf("观察者保存的末帧 finish_reason 被就地改写: %q（want stop）", fr)
	}
}

// TestFrameObserverDoesNotAlterPassthroughBytes 传副本不得改变透传字节。
func TestFrameObserverDoesNotAlterPassthroughBytes(t *testing.T) {
	base := httptest.NewRecorder()
	if err := Stream(base, strings.NewReader(sseFixture)); err != nil {
		t.Fatal(err)
	}
	withObs := httptest.NewRecorder()
	if err := StreamHint(withObs, strings.NewReader(sseFixture), nil,
		WithFrameObserver(func(map[string]any) {}),
	); err != nil {
		t.Fatal(err)
	}
	if base.Body.String() != withObs.Body.String() {
		t.Fatal("挂观察者不得改变透传字节")
	}
}

// TestCloneFrameIsDeepForDelta cloneFrame 必须深拷 delta（被改写的两处之一）。
func TestCloneFrameIsDeepForDelta(t *testing.T) {
	src := map[string]any{
		"id": "c1",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": "orig", "role": "assistant"},
			"finish_reason": "stop",
		}},
	}
	dst := cloneFrame(src)

	// 改写源（模拟 repairFrame 就地改写）。
	dc, _ := src["choices"].([]any)[0].(map[string]any)
	dd, _ := dc["delta"].(map[string]any)
	dd["content"] = "repaired"
	dc["finish_reason"] = "tool_calls"

	// 副本必须不受影响。
	cc, _ := dst["choices"].([]any)[0].(map[string]any)
	cd, _ := cc["delta"].(map[string]any)
	if got, _ := cd["content"].(string); got != "orig" {
		t.Fatalf("cloneFrame 未深拷 delta: %q（want orig）", got)
	}
	if fr, _ := cc["finish_reason"].(string); fr != "stop" {
		t.Fatalf("cloneFrame 未深拷 finish_reason: %q（want stop）", fr)
	}
}

// TestCloneFrameHandlesNonChoiceFrames 非标准帧（无 choices）不得 panic。
func TestCloneFrameHandlesNonChoiceFrames(t *testing.T) {
	if cloneFrame(nil) != nil {
		t.Fatal("cloneFrame(nil) 应返回 nil")
	}
	got := cloneFrame(map[string]any{"error": map[string]any{"code": 11128}})
	if got == nil {
		t.Fatal("cloneFrame 对 error 帧应返回副本")
	}
	e, ok := got["error"].(map[string]any)
	if !ok || e["code"] != 11128 {
		t.Fatalf("cloneFrame 丢失了 error 内容: %+v", got)
	}
	// error 帧也必须**深拷**：改写**源**不得影响副本（浅拷贝会共享同一个 map）。
	src := map[string]any{"error": map[string]any{"code": 11128}}
	dst := cloneFrame(src)
	de, _ := src["error"].(map[string]any)
	de["code"] = 99999
	if ce, _ := dst["error"].(map[string]any); ce["code"] != 11128 {
		t.Fatalf("cloneFrame 未深拷 error 对象: %+v", dst)
	}
}
