package server

import (
	"fmt"
	"testing"
	"time"
)

// restoreVars 备份/恢复 restrict.go 的包级可调参数（测试用短参数，清理时还原，
// 避免影响同包其他测试）。
func restoreVars(t *testing.T, fns ...func()) {
	t.Helper()
	t.Cleanup(func() {
		for _, fn := range fns {
			fn()
		}
	})
}

// timeVar 生成「保存旧值 + 返回还原闭包 + 设置新值」的模板，减少样板。
func timeVar(p *time.Duration, v time.Duration) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func intVar(p *int, v int) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

// TestRestrictionThresholdScalesWithPool 连坐阈值 = 该域可用号数 / 除数，下限 2：
// 小池封两个就暂停（保护要硬），大池按比例放大（340 个健康号封 2 个就躺 30 分钟
// 显然是误判）。
func TestRestrictionThresholdScalesWithPool(t *testing.T) {
	cases := []struct {
		healthy int
		div     int
		want    int
	}{
		{0, 10, 2},    // 池空：下限
		{3, 10, 2},    // 小池：下限
		{10, 10, 2},   // 10/10=1 → 下限
		{19, 10, 2},   // 19/10=1 → 下限
		{20, 10, 2},   // 20/10=2 → 恰好等于下限
		{50, 10, 5},   // 大池按比例
		{340, 10, 34}, // 340/10
		{0, 0, 0},     // div=0：关闭连坐闸
		{340, 0, 0},   // div=0：即使大池也关闭
	}
	for _, tc := range cases {
		if got := restrictionThreshold(tc.healthy, tc.div); got != tc.want {
			t.Errorf("restrictionThreshold(%d, %d)=%d want %d", tc.healthy, tc.div, got, tc.want)
		}
	}
}

// TestRestrictionWaveIgnoresSmallBanRateOnBigPool 大池的误判保护：340 个健康号里
// 封了 2 个不暂停（偶发风控不放大到整域）；封到 34 个（阈值）才暂停。
func TestRestrictionWaveIgnoresSmallBanRateOnBigPool(t *testing.T) {
	g := &restrictionGate{}
	for i := 0; i < 2; i++ {
		if g.note("cn", fmt.Sprintf("u%d", i), 30*time.Minute, 340, 10) {
			t.Fatalf("hit #%d triggered the wave: 2 bans out of 340 healthy must not pause the realm", i+1)
		}
	}
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("remaining=%v want 0", d)
	}
	// 阈值 34：第 3..33 个仍不触发，第 34 个触发。
	for i := 2; i < 33; i++ {
		if g.note("cn", fmt.Sprintf("u%d", i), 30*time.Minute, 340, 10) {
			t.Fatalf("hit #%d triggered too early (threshold should be 34)", i+1)
		}
	}
	if !g.note("cn", "u33", 30*time.Minute, 340, 10) {
		t.Fatal("hit #34 must trigger the wave")
	}
	if d := g.remaining("cn"); d <= 0 {
		t.Fatalf("remaining=%v want > 0 after the wave", d)
	}
}

// TestRestrictionWaveCountsDistinctAccounts 同一个号重复报错只算一次：否则一个
// 坏号连撞两次就能把整个域暂停（这个保护的成本是全域停服，判据必须严格）。
func TestRestrictionWaveCountsDistinctAccounts(t *testing.T) {
	g := &restrictionGate{}
	for i := 0; i < 5; i++ {
		if g.note("cn", "same-uid", 30*time.Minute, 10, 10) {
			t.Fatalf("repeat #%d of the same uid triggered the wave", i+1)
		}
	}
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("remaining=%v want 0 (one account is not a wave)", d)
	}
}

// TestRestrictionHoldEscalates 连续连坐按 2 倍退避：30s → 1m → 2m → 4m，封顶
// restrictionHoldMax。安静一会儿又开始被封 = 还在同一波风控里，短窗试探只会再
// 送几个号进去。
func TestRestrictionHoldEscalates(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	g := &restrictionGate{}
	uid := 0
	for i, w := range want {
		for j := 0; j < restrictionWaveMin; j++ {
			uid++
			g.note("cn", fmt.Sprintf("u%d", uid), 30*time.Minute, 10, 10)
		}
		got := g.remaining("cn")
		if got <= 0 {
			t.Fatalf("streak %d: remaining=%v want > 0", i+2, got)
		}
		// 允许毫秒级偏差（remaining 是「从现在到 until」，总比 pause 略小）。
		if got > w || got < w-time.Second {
			t.Fatalf("streak %d: remaining=%v want ~%v", i+2, got, w)
		}
	}
	// 第 5、6 波继续翻倍并封顶 10m。
	for i := 0; i < 4; i++ {
		for j := 0; j < restrictionWaveMin; j++ {
			uid++
			g.note("cn", fmt.Sprintf("u%d", uid), time.Hour, 10, 10)
		}
	}
	if got := g.remaining("cn"); got > restrictionHoldMax || got < restrictionHoldMax-time.Second {
		t.Fatalf("remaining=%v want capped at %v", got, restrictionHoldMax)
	}
}

// TestRestrictionHoldConfigShorterWins 配置比退避更短时按配置：用户显式写了
// restriction_hold_seconds=5，就不能因为 streak 高涨到 2 分钟。
func TestRestrictionHoldConfigShorterWins(t *testing.T) {
	g := &restrictionGate{}
	uid := 0
	for wave := 0; wave < 3; wave++ {
		for j := 0; j < restrictionWaveMin; j++ {
			uid++
			g.note("cn", fmt.Sprintf("u%d", uid), 5*time.Second, 10, 10)
		}
		if got := g.remaining("cn"); got > 5*time.Second {
			t.Fatalf("wave %d: remaining=%v want <= 5s (config wins over escalation)", wave+1, got)
		}
	}
}

// TestRestrictionStreakResetsAfterCalm 平静 restrictionCalmReset 之后退避清零：
// 风控波已过，下一波重新从 30s 起（否则一次长封之后永远是最长暂停）。
func TestRestrictionStreakResetsAfterCalm(t *testing.T) {
	restoreVars(t, timeVar(&restrictionCalmReset, 10*time.Millisecond))
	g := &restrictionGate{}
	uid := 0
	wave := func() time.Duration {
		for j := 0; j < restrictionWaveMin; j++ {
			uid++
			g.note("cn", fmt.Sprintf("u%d", uid), 30*time.Minute, 10, 10)
		}
		return g.remaining("cn")
	}
	if got := wave(); got > restrictionHoldBase || got < restrictionHoldBase-time.Second {
		t.Fatalf("first wave remaining=%v want ~%v", got, restrictionHoldBase)
	}
	if got := wave(); got > 2*restrictionHoldBase || got < 2*restrictionHoldBase-time.Second {
		t.Fatalf("second wave remaining=%v want ~%v (escalated)", got, 2*restrictionHoldBase)
	}
	time.Sleep(20 * time.Millisecond)
	if got := wave(); got > restrictionHoldBase || got < restrictionHoldBase-time.Second {
		t.Fatalf("after calm remaining=%v want back to ~%v", got, restrictionHoldBase)
	}
}

// TestRestrictionWaveWindowPrunesHits 窗口外的命中不再参与计数：隔了很久的两条
// 命中不是「同一波」，不该叠加成暂停。
func TestRestrictionWaveWindowPrunesHits(t *testing.T) {
	restoreVars(t, timeVar(&restrictionWaveWindow, 0))
	g := &restrictionGate{}
	if g.note("cn", "u1", 30*time.Minute, 10, 10) {
		t.Fatal("first hit must not trigger")
	}
	// 窗口为 0：上一条命中在加锁后立刻过期（prune 先于计数）→ 永远凑不到阈值。
	if g.note("cn", "u2", 30*time.Minute, 10, 10) {
		t.Fatal("hit outside the window must not accumulate into a wave")
	}
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("remaining=%v want 0", d)
	}
}

// TestRestrictionGateRealmIsolation 连坐状态按域隔离：cn 暂停不影响 global。
func TestRestrictionGateRealmIsolation(t *testing.T) {
	g := &restrictionGate{}
	for j := 0; j < restrictionWaveMin; j++ {
		g.note("cn", fmt.Sprintf("cn%d", j), 30*time.Minute, 10, 10)
	}
	if d := g.remaining("cn"); d <= 0 {
		t.Fatalf("cn remaining=%v want > 0", d)
	}
	if d := g.remaining("global"); d != 0 {
		t.Fatalf("global remaining=%v want 0 (per-realm state)", d)
	}
	for j := 0; j < restrictionWaveMin; j++ {
		g.note("global", fmt.Sprintf("g%d", j), 30*time.Minute, 10, 10)
	}
	if d := g.remaining("global"); d <= 0 {
		t.Fatalf("global remaining=%v want > 0", d)
	}
}

// TestRestrictionGateZeroValueUsable 零值可用（Handler 内嵌字段没有构造函数）：
// 未初始化时 remaining()==0、note() 在 hold=0 时只是空操作。
func TestRestrictionGateZeroValueUsable(t *testing.T) {
	var g restrictionGate
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("remaining=%v want 0 on zero value", d)
	}
	for j := 0; j < 5; j++ {
		if g.note("cn", fmt.Sprintf("u%d", j), 0, 0, 10) {
			t.Fatal("hold=0 must never pause dispatch")
		}
	}
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("remaining=%v want 0", d)
	}
	if g.note("cn", "", time.Hour, 10, 10) {
		t.Fatal("empty uid must not count")
	}
}

// TestRestrictionThresholdDisabledByDiv 除数 <= 0 = 关闭连坐闸（只留停手），
// note 必须直接返回 false 且不落任何状态。
func TestRestrictionThresholdDisabledByDiv(t *testing.T) {
	g := &restrictionGate{}
	for j := 0; j < 10; j++ {
		if g.note("cn", fmt.Sprintf("u%d", j), time.Hour, 10, 0) {
			t.Fatalf("hit #%d paused dispatch with div=0", j+1)
		}
	}
	if d := g.remaining("cn"); d != 0 {
		t.Fatalf("remaining=%v want 0", d)
	}
	if len(g.m) != 0 {
		t.Errorf("gate state=%v want empty (div=0 must not accumulate)", g.m)
	}
}

// TestRealmHealthyCountsOnlyUsable realmHealthy 是阈值的基数：只数该域可用号，
// 禁用/冷却的号不参与（否则连坐阈值会随着封号而越涨越高，等于永不暂停）。
func TestRealmHealthyCountsOnlyUsable(t *testing.T) {
	p := cnAccounts(t)
	h := NewHandler(Config{Pool: p})
	if got := h.realmHealthy("cn"); got != 2 {
		t.Fatalf("realmHealthy(cn)=%d want 2", got)
	}
	p.Disable("i1", "test")
	if got := h.realmHealthy("cn"); got != 1 {
		t.Fatalf("realmHealthy(cn)=%d want 1 after disabling one account", got)
	}
	if got := h.realmHealthy("global"); got != 0 {
		t.Fatalf("realmHealthy(global)=%d want 0", got)
	}
	if got := (&Handler{}).realmHealthy("cn"); got != 0 {
		t.Fatalf("realmHealthy with nil pool=%d want 0", got)
	}
}
