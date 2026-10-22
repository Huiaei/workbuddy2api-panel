package pool

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestBlockModelBusyShortLadder 14003 的避让必须是**秒级**：首档 10s，
// 逐次翻倍但封顶 1m。把间歇抖动停成整分钟不可用正是 issue #46 的实案。
func TestBlockModelBusyShortLadder(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}
	for i, w := range want {
		before := time.Now()
		p.BlockModelBusy("u1", "m1", "upstream said busy")
		st := p.ModelBlocked("m1")
		if !st.Blocked {
			t.Fatalf("第 %d 次后应处于全池模型级避让", i+1)
		}
		got := st.Until.Sub(before)
		// 允许 1s 的执行误差
		if got < w || got > w+time.Second {
			t.Fatalf("第 %d 次 TTL=%v want≈%v", i+1, got, w)
		}
		if !strings.HasPrefix(st.Reason, "14003") {
			t.Fatalf("reason 必须以 14003 开头，got=%q", st.Reason)
		}
		if !st.Transient {
			t.Fatalf("14003 条目必须标为瞬时（可重试语义）")
		}
	}
}

// TestModelBlockedTransientOnlyWhenAllTransient 只要有一个号是确定性的（11102），
// 整体就不能报「可重试」——那种情况下重试一定还是同样失败。
func TestModelBlockedTransientOnlyWhenAllTransient(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	p.BlockModelBusy("u1", "m1", "busy")
	p.BlockModelBusy("u2", "m1", "busy")
	if st := p.ModelBlocked("m1"); !st.Blocked || !st.Transient {
		t.Fatalf("两个号都是 14003 → Blocked=true Transient=true，got=%+v", st)
	}

	p.BlockModelBackoff("u2", "m1", "11102 model not found")
	if st := p.ModelBlocked("m1"); !st.Blocked || st.Transient {
		t.Fatalf("混入 11102 → 必须 Transient=false，got=%+v", st)
	}
}

// TestBlockModelClearClearsBusy 实测通了就该立刻解除 14003 避让（不等 10s 到期）。
func TestBlockModelClearClearsBusy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBusy("u1", "m1", "busy")
	p.BlockModelClear("u1", "m1")
	if st := p.ModelBlocked("m1"); st.Blocked {
		t.Fatalf("成功一次后不应再被 14003 挡住，got=%+v", st)
	}
}
