package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件钉住**交叉审计第二轮 Q5.3** 抓到的契约缺陷：
//
// 「观察者只读不改对象」这条承诺**只对同步读取成立**——修复/规范化阶段拿到的
// 就是同一个 map 引用，它会就地改写 delta.content 与 finish_reason。任何把帧
// 对象**存下来**的观察者，事后读到的都是被改写后的状态。
//
// 修法：交给观察者的是**深拷贝**，观察者拿到的永远是「上游原样」。
//
// ⚠️ v1.13.0 基线分支**没有 DSML 修复层**（internal/upstream/dsml.go 不存在），
// 所以这里不构造「会被修复层改写」的夹具，而是直接钉住**契约本身**：
//  1) cloneFrame 必须是**深**拷贝（delta / finish_reason / error 都得隔离）；
//  2) 观察者拿到的帧必须与上游原始帧一致。
// 其中 (1) 是可证伪的：浅拷贝会共享同一个 delta map，测试必然失败。

// observerFrameFixture 一条普通的两帧流：首帧有正文，末帧 finish_reason=stop。
func observerFrameFixture() string {
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
	return mk("你好", "") + mk("", "stop") + "data: [DONE]\n\n"
}

// TestFrameObserverReceivesUnmodifiedFrame 观察者拿到的帧必须是上游原样。
func TestFrameObserverReceivesUnmodifiedFrame(t *testing.T) {
	var kept []map[string]any
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(observerFrameFixture()), nil,
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
	first := kept[0]
	chs, _ := first["choices"].([]any)
	if len(chs) == 0 {
		t.Fatal("首帧缺少 choices")
	}
	c0, _ := chs[0].(map[string]any)
	d0, _ := c0["delta"].(map[string]any)
	if txt, _ := d0["content"].(string); txt != "你好" {
		t.Fatalf("观察者保存的帧内容被改写了: %q（want 你好）", txt)
	}
	last := kept[len(kept)-1]
	lchs, _ := last["choices"].([]any)
	lc, _ := lchs[0].(map[string]any)
	if fr, _ := lc["finish_reason"].(string); fr != "stop" {
		t.Fatalf("观察者保存的末帧 finish_reason 被改写: %q（want stop）", fr)
	}
}

// TestFrameObserverDoesNotAlterPassthroughBytes 挂观察者不得改变透传字节。
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

// TestCloneFrameIsDeepForDelta cloneFrame 必须深拷 delta 与 finish_reason。
//
// 这是本文件里**唯一可证伪**的断言：若 cloneFrame 只做浅拷贝（复用同一个
// delta map），改写源之后副本会跟着变 ⇒ 失败。
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

	// 改写源（模拟修复/规范化阶段的就地改写）。
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
	// error 帧也必须深拷（否则观察者与错误处理共享同一对象）。
	src := map[string]any{"error": map[string]any{"code": 11128}}
	dst := cloneFrame(src)
	de, _ := src["error"].(map[string]any)
	de["code"] = 99999
	if ce, _ := dst["error"].(map[string]any); ce["code"] != 11128 {
		t.Fatalf("cloneFrame 未深拷 error 对象: %+v", dst)
	}
}
