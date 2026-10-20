// burnguard.go 积分燃尽熔断（issue #229）。
//
// 问题：某些模型（实测 hy4-preview）单轮 140k 上下文就要烧 20+ 积分，
// 而前缀缓存命中率极低（26%）。用户一个长会话下来，积分肉眼可见地蒸发，
// 但网关此前**没有任何额度护栏**——只管把请求转给上游。
//
// 诉求（issue #229 作者原话）：「短时间消耗积分过多 / 连续多轮缓存率极低
// → 触发时拒绝继续对话」。
//
// 做法：在**账号**维度做滑动窗口计账。窗口内累计烧掉的积分超过阈值时，
// 该账号进入 burn-cooldown（软冷却的一种），在一段时间内不再被选号命中；
// 面板显式展示原因，用户可手动解除（暂停/恢复选号或刷新余额即清零）。
//
// 设计取舍：
//   - 熔断是**账号级**而非全局级：池里还有其它账号时，请求照常走别的号，
//     不会让整个网关不可用（真到"全部烧完"时自然会 no_healthy_account）。
//   - 阈值默认 0 = 关闭（零回归）：不配置就不熔断，行为与引入前完全一致。
//   - 只统计**有上游 credit 观测**的消耗：免费请求（credit=0）不计。
//   - 冷却时长随倍率递增（线性封顶），避免"刚好过线就恢复、马上又烧穿"。

package pool

import (
	"log"
	"strconv"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// burnWindow 单账号的滑动窗口计账。
type burnWindow struct {
	// windowStart 窗口起点（窗口滚动时重置）。
	windowStart time.Time
	// spent 窗口内累计烧掉的积分。
	spent float64
	// trips 触发熔断次数（用于冷却时长递增）。
	trips int
	// until 熔断解除时刻（零值 = 未熔断）。
	until time.Time
}

// burnGuard 参数默认值。
const (
	// defaultBurnGuardLimit 窗口内允许烧掉的积分上限（0 = 关闭熔断）。
	defaultBurnGuardLimit = 0.0
	// defaultBurnGuardWindow 计账窗口长度。
	defaultBurnGuardWindow = 10 * time.Minute
	// defaultBurnGuardCooldown 首次熔断的冷却时长。
	defaultBurnGuardCooldown = 10 * time.Minute
	// defaultBurnGuardCooldownMax 冷却时长上限。
	defaultBurnGuardCooldownMax = 2 * time.Hour
)

// SetBurnGuard 注入积分燃尽熔断参数（issue #229）。
//
//   - limit：窗口内允许烧掉的积分上限；<= 0 = 关闭（默认，零回归）。
//   - window：计账窗口；<= 0 = 用默认 10m。
//   - cooldown / cooldownMax：熔断冷却时长与上限；<= 0 = 用默认。
func (p *Pool) SetBurnGuard(limit float64, window, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.burnGuardLimit = limit
	if window <= 0 {
		window = defaultBurnGuardWindow
	}
	p.burnGuardWindow = window
	if cooldown <= 0 {
		cooldown = defaultBurnGuardCooldown
	}
	p.burnGuardCooldown = cooldown
	if cooldownMax <= 0 {
		cooldownMax = defaultBurnGuardCooldownMax
	}
	p.burnGuardCooldownMax = cooldownMax
}

// NoteBurn 记录一次积分消耗并在超限时熔断该账号（issue #229）。
//
// credit <= 0（免费请求）不计账。窗口过期后重新起窗。
// 返回 true 表示本次调用触发了熔断。
func (p *Pool) NoteBurn(uid string, credit float64) bool {
	if uid == "" || credit <= 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.burnGuardLimit <= 0 {
		return false // 熔断未启用
	}
	if _, ok := p.byUID[uid]; !ok {
		return false
	}
	now := time.Now()
	bw := p.burnGuard[uid]
	if bw == nil {
		bw = &burnWindow{windowStart: now}
		p.burnGuard[uid] = bw
	}
	// 窗口滚动：窗口长度已过 → 重置计账（但不重置熔断状态 trips/until）。
	//
	// 注意：这里判的是「**已过去一个窗口**」，而不是「距上次记账已过一个窗口」：
	// 后者会让连续小额的请求永远滚不出窗口（每笔都把 windowStart 推到当下，
	// spent 从不清零）——那正是熔断要防的场景。窗口是**固定起点**的滚动窗口，
	// 超限判定读的是该窗口内的累计值。
	if now.Sub(bw.windowStart) >= p.burnGuardWindow {
		bw.windowStart = now
		bw.spent = 0
	}
	bw.spent += credit
	if bw.spent <= p.burnGuardLimit {
		return false
	}
	// 超限：熔断。冷却时长随触发次数递增（线性，封顶）。
	bw.trips++
	dur := p.burnGuardCooldown
	if extra := time.Duration(bw.trips-1) * p.burnGuardCooldown; extra > 0 {
		dur += extra
	}
	if max := p.burnGuardCooldownMax; dur > max {
		dur = max
	}
	bw.until = now.Add(dur)
	log.Printf("WARN: [pool] burn guard: acct=%s spent=%.1f credits in %s (limit=%.1f) → 熔断 %s",
		logfmt.UID8(uid), bw.spent, p.burnGuardWindow, p.burnGuardLimit, dur)
	return true
}

// burnBlocked 报告账号是否处于积分燃尽熔断中。
func (p *Pool) burnBlocked(e *entry, now time.Time) bool {
	if p.burnGuardLimit <= 0 || e == nil {
		return false
	}
	bw := p.burnGuard[e.a.UID]
	if bw == nil || bw.until.IsZero() {
		return false
	}
	return bw.until.After(now)
}

// burnReason 返回熔断原因文案（未熔断返回空串）。
func (p *Pool) burnReason(uid string, now time.Time) string {
	if p.burnGuardLimit <= 0 {
		return ""
	}
	bw := p.burnGuard[uid]
	if bw == nil || bw.until.IsZero() || !bw.until.After(now) {
		return ""
	}
	return "积分燃尽熔断：窗口内消耗 " + trimFloat(bw.spent) + " 积分超过阈值 " + trimFloat(p.burnGuardLimit) +
		"，冷却至 " + bw.until.Format("15:04:05")
}

// trimFloat 去掉浮点尾巴（面板文案用）。
func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
