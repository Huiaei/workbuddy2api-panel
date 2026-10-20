package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamMultipleFrameObservers 多个逐帧观察者可以共存，**不会被静默覆盖**。
//
// 此前 WithFrameObserver 是单槽位（后注册覆盖先注册），排查这类问题时极难
// 发现「我的观察者根本没被调用」——现象是「判定偶尔不生效」，且没有报错。
func TestStreamMultipleFrameObservers(t *testing.T) {
	rec := httptest.NewRecorder()
	var seen1, seen2 int
	err := StreamHint(rec, strings.NewReader(sseFixture), nil,
		WithFrameObserver(func(obj map[string]any) { seen1++ }),
		WithFrameObserver(func(obj map[string]any) { seen2++ }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if seen1 == 0 {
		t.Errorf("第一个观察者没被调用")
	}
	if seen2 == 0 {
		t.Errorf("第二个观察者没被调用（被前一个静默覆盖了）")
	}
	if seen1 != seen2 {
		t.Errorf("两个观察者应看到同样多的帧: %d vs %d", seen1, seen2)
	}
}

// TestStreamFrameObserverDoesNotAlterBytes 观察者是纯旁路：透传字节必须与不注册
// 观察者时**逐字节相同**。
func TestStreamFrameObserverDoesNotAlterBytes(t *testing.T) {
	base := httptest.NewRecorder()
	if err := Stream(base, strings.NewReader(sseFixture)); err != nil {
		t.Fatal(err)
	}
	withObs := httptest.NewRecorder()
	if err := StreamHint(withObs, strings.NewReader(sseFixture), nil,
		WithFrameObserver(func(obj map[string]any) { /* 只读不改 */ }),
		WithFrameObserver(func(obj map[string]any) { /* 只读不改 */ }),
	); err != nil {
		t.Fatal(err)
	}
	if base.Body.String() != withObs.Body.String() {
		t.Errorf("注册观察者后透传字节变了:\n无=%q\n有=%q", base.Body.String(), withObs.Body.String())
	}
}
