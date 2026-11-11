package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestRoutingWeightNeverBelowBaseline 钉住「开启到期加权永不低于关闭时的权重」
// 这条硬不变量：勾了「快过期积分优先」，账号只可能被加权，绝不能因为开启而
// 更少被选中。
//
// 参考实现（reporter fork 的 2af7de6）修的正是这个坑：那边把余额项折算到
// creditScale=0.2 再乘紧迫度，窗口边缘紧迫度只有 1.5 ⇒ (1+2)×1.5=4.5，而不开
// 时的基线是 (1+10)=11，落在窗口最后 22% 里「开了比不开还差」。
//
// 本实现的倍率恒 ≥ expiringBaseMultiplier(1.5) 且只做乘法，天然不反降权；这
// 条测试把该不变量固化下来，将来若改成折算口径会立刻变红。
func TestRoutingWeightNeverBelowBaseline(t *testing.T) {
	const win = 168 * time.Hour
	now := time.Now()

	p := New("")
	p.Add(&auth.Auth{UID: "rich"})
	p.SetPreferExpiring(true)
	p.SetExpiringSoonWindow(win)
	// 余额取满（credits 项吃满 10）、刚用过（无闲置补偿）：权重最低的形态。
	p.SetCreditsDetailed("rich", 500, 500, 500, now.Add(win-time.Hour), 500)

	p.mu.Lock()
	e := p.byUID["rich"]
	e.lastUsed = now
	base := p.weightOf(e, 500, now)
	got := p.routingWeightOf(e, 500, now)
	u := p.expiringUrgency(e, now)
	p.mu.Unlock()

	if got < base {
		t.Fatalf("窗口内账号被反降权: prefer=on 权重=%v < 基线=%v (urgency=%v)", got, base, u)
	}
}

// TestRoutingWeightNeverBelowBaselineAcrossUrgency 把同一条不变量铺到整个紧迫度
// 梯度上：窗口刚进入、窗口中点、临近到期三种位置，加权后都不得低于基线。
func TestRoutingWeightNeverBelowBaselineAcrossUrgency(t *testing.T) {
	const win = 168 * time.Hour
	now := time.Now()

	cases := []struct {
		name  string
		until time.Time
	}{
		{"窗口边缘", now.Add(win)},
		{"窗口中点", now.Add(win / 2)},
		{"临近到期", now.Add(time.Minute)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "a"})
			p.SetPreferExpiring(true)
			p.SetExpiringSoonWindow(win)
			p.SetCreditsDetailed("a", 100, 100, 50, c.until, 50)

			p.mu.Lock()
			e := p.byUID["a"]
			e.lastUsed = now
			base := p.weightOf(e, 100, now)
			got := p.routingWeightOf(e, 100, now)
			p.mu.Unlock()

			if got < base {
				t.Fatalf("%s: 加权后低于基线 got=%v base=%v", c.name, got, base)
			}
			if got <= base {
				t.Fatalf("%s: 窗口内账号应被加权（严格大于基线）got=%v base=%v", c.name, got, base)
			}
		})
	}
}

// TestRoutingWeightUnchangedWhenNoExpiringBatch：账号没有有效快过期批次时，
// 加权口径与关闭开关完全一致（倍率 1，不动权重）。
func TestRoutingWeightUnchangedWhenNoExpiringBatch(t *testing.T) {
	const win = 168 * time.Hour
	now := time.Now()

	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.SetPreferExpiring(true)
	p.SetExpiringSoonWindow(win)
	// 无快过期批次：creditsExpiring=0 ⇒ expiringNow=false。
	p.SetCreditsDetailed("a", 100, 100, 0, time.Time{}, 0)

	p.mu.Lock()
	e := p.byUID["a"]
	base := p.weightOf(e, 100, now)
	got := p.routingWeightOf(e, 100, now)
	p.mu.Unlock()

	if got != base {
		t.Fatalf("无快过期批次时权重应等于基线 got=%v base=%v", got, base)
	}
}
