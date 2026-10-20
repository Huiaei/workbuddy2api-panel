package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestBurnGuardDisabledByDefault 验证默认关闭熔断（零回归）：
// 不配置阈值时，记再多的消耗也不会熔断，账号始终可选。
func TestBurnGuardDisabledByDefault(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1"}
	p.Add(a)
	// 连续烧 1000 积分
	for i := 0; i < 100; i++ {
		if p.NoteBurn(a.UID, 10) {
			t.Fatalf("熔断在未配置阈值时不应触发（第 %d 次）", i)
		}
	}
	if got := p.Pick(); got == nil {
		t.Fatal("未启用熔断时账号应仍可被选中")
	}
}

// TestBurnGuardTripsOnLimit 验证超限熔断：窗口内累计消耗超过阈值后，
// 该账号不再被 pick 选中，Status 透出 burn 原因。
func TestBurnGuardTripsOnLimit(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1"}
	p.Add(a)
	p.SetBurnGuard(50, time.Minute, time.Minute, time.Hour)

	if p.NoteBurn(a.UID, 20) {
		t.Fatal("未达阈值不应熔断")
	}
	if p.Pick() == nil {
		t.Fatal("未熔断前账号应可被选中")
	}
	if !p.NoteBurn(a.UID, 40) {
		t.Fatal("累计 60 > 阈值 50 应触发熔断")
	}
	if p.Pick() != nil {
		t.Fatal("熔断期内账号不应被选中")
	}
	st := p.statusOf(a.UID, p.byUID[a.UID])
	if st.CoolKind != "burn" {
		t.Fatalf("CoolKind = %q，期望 burn", st.CoolKind)
	}
	if st.Reason == "" {
		t.Fatal("熔断原因应透出到 Status.Reason")
	}
}

// TestBurnGuardWindowRollover 验证窗口滑动：窗口过期后重新计账，
// 不再处于熔断状态（直到再次超限）。
func TestBurnGuardWindowRollover(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1"}
	p.Add(a)
	p.SetBurnGuard(50, 50*time.Millisecond, time.Minute, time.Hour)

	if !p.NoteBurn(a.UID, 60) {
		t.Fatal("应触发熔断")
	}
	// 窗口过期 → 重新起窗计账；再次熔断需要新的累计超限
	time.Sleep(60 * time.Millisecond)
	if p.NoteBurn(a.UID, 10) {
		t.Fatal("窗口滚动后 10 积分不应再次触发熔断")
	}
}
