package server

import (
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 11140 连坐保护（restrict）。
//
// 上游「账号存在异常行为 / unusual activity」限制文书（判据见
// upstream.IsRestricted）一旦出现，说明这批号已经进了风控名单。此时正确的反应
// 不是「换个号再试」，而是**停手**——上游的风控以请求指纹 + 出口 IP 为单位判定，
// 同一个请求顺着轮转挨个号去撞，只会把整池号一起送进名单（实测一次请求能在
// 几十秒内把 5 个健康号全打成 11140）。
//
// 两道闸：
//   - 第一道（停手，本请求生效）：命中即禁用该号 + 本请求不再换号。由
//     noteRestriction 返回 true、轮转循环据此 break 实现（见 handler.go 的
//     非流式尾段）。
//   - 第二道（连坐，跨请求生效）：同一域的被封号数在 restrictionWaveWindow 内
//     达到阈值 → 暂停该域派发 restrictionHold（upstream.restriction_hold_seconds，
//     缺省 1800s）。阈值 = 该域此刻可用号数 / 连坐除数
//     （upstream.restriction_wave_div，缺省 10），下限 restrictionWaveMin = 2：
//     小池封两个就触发，大池按比例放大——340 个健康号的池子封 2 个不暂停、封到
//     34 个才暂停（否则大池会在「封了 2 个」时直接躺 30 分钟）。
//
// 为什么按域（cn/global）分开计数：两域是两套完全独立的上游（不同域名、不同
// 风控名单、不同出口）。cn 被连坐时 global 往往还健康，一起暂停等于人为放大
// 故障面。
//
// 退避：暂停时长从 restrictionHoldBase（30s）起按 2 倍翻、封顶
// restrictionHoldMax（10min）——「安静了一会儿又开始被封」说明还在同一波风控里，
// 短窗试探只会再送几个号进去。平静 restrictionCalmReset（30min）后清零，重新
// 从 30s 起（风控波已过，没必要继续长暂停）。
//
// 并发：进程内状态、重启清零（与 wafip.go 同口径：这类保护只在「几分钟内不许
// 再撞」这个尺度上有意义，不值得持久化）。选号是并发的，故所有读写走互斥锁。
var (
	// restrictionWaveWindow 连坐计数窗口：窗口内的封号数才算「同一波」。
	restrictionWaveWindow = 15 * time.Minute
	// restrictionWaveMin 连坐阈值下限（小池保护：池里只有 10 个号时阈值也是 2，
	// 不因除数把阈值压到 1 —— 一次偶发 11140 就暂停整个域太激进）。
	restrictionWaveMin = 2
	// restrictionHoldBase 首次连坐的暂停时长。
	restrictionHoldBase = 30 * time.Second
	// restrictionHoldMax 连续连坐的暂停时长上限。
	restrictionHoldMax = 10 * time.Minute
	// restrictionCalmReset 平静多久之后退避清零。
	restrictionCalmReset = 30 * time.Minute
)

// restrictionThreshold 返回「同域同窗内被封号数达到多少 → 暂停该域派发」。
//
// div 来自 upstream.restriction_wave_div（缺省 10）；div <= 0 = 关闭连坐闸，
// 返回 0（调用方据此跳过）。阈值下限为 restrictionWaveMin。
func restrictionThreshold(healthy, div int) int {
	if div <= 0 {
		return 0
	}
	if n := healthy / div; n > restrictionWaveMin {
		return n
	}
	return restrictionWaveMin
}

// restrictionState 单域的连坐状态。
type restrictionState struct {
	// hits 窗口内被封的号（UID → 命中时刻）。同一号重复命中只保留首次时刻。
	hits map[string]time.Time
	// until 派发暂停的截止时刻（零值 = 未暂停）。
	until time.Time
	// streak 连续连坐波数（决定退避档位）。
	streak int
	// last 上一次触发连坐的时刻（供平静期判定）。
	last time.Time
}

// restrictionGate 连坐闸门。零值可用（map 惰性建）。
type restrictionGate struct {
	mu sync.Mutex
	m  map[string]*restrictionState
}

// note 记一次「某域某号被上游限制」，返回是否因此触发连坐暂停。
//
// hold = 连坐暂停时长（upstream.restriction_hold_seconds）。hold <= 0 = 只保留
// 第一道闸（停手）：照常计数并打日志，但永不暂停派发。
//
// healthy = 该域此刻可用号数，div = 连坐除数（<=0 = 关闭连坐闸）。
func (g *restrictionGate) note(realm, uid string, hold time.Duration, healthy, div int) bool {
	if uid == "" || hold <= 0 {
		return false
	}
	need := restrictionThreshold(healthy, div)
	if need <= 0 {
		return false
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*restrictionState{}
	}
	st := g.m[realm]
	if st == nil {
		st = &restrictionState{}
		g.m[realm] = st
	}
	if st.hits == nil {
		st.hits = map[string]time.Time{}
	}
	// 惰性剪枝：窗口外的旧命中不再参与计数。不用后台 goroutine——闸门只在被撞
	// 到时才有写入，顺手剪一遍足够，且没有生命周期要管。
	for id, t := range st.hits {
		if now.Sub(t) > restrictionWaveWindow {
			delete(st.hits, id)
		}
	}
	// 同一号只记一次：否则一个号连续报错就能把计数顶到阈值，把整个域暂停。
	if _, ok := st.hits[uid]; !ok {
		st.hits[uid] = now
	}
	if len(st.hits) < need {
		return false
	}
	// 平静期已过 → 退避清零（这是新的一波，没必要时长接着上一波翻倍）。
	if !st.last.IsZero() && now.Sub(st.last) > restrictionCalmReset {
		st.streak = 0
	}
	st.streak++
	pause := restrictionHoldBase
	for i := 1; i < st.streak && pause < restrictionHoldMax; i++ {
		pause *= 2
	}
	if pause > restrictionHoldMax {
		pause = restrictionHoldMax
	}
	// 配置值优先：配置比退避更短时按配置（用户显式说了只暂停这么久）。
	if pause > hold {
		pause = hold
	}
	st.until = now.Add(pause)
	st.last = now
	st.hits = map[string]time.Time{}
	log.Printf("WARN: [server] upstream restriction wave realm=%q: %d accounts restricted within %s (threshold %d, %d healthy) — dispatch paused for %s (streak %d)",
		realm, need, restrictionWaveWindow, need, healthy, pause, st.streak)
	return true
}

// remaining 返回该域剩余的派发暂停时长；0 = 未暂停。
func (g *restrictionGate) remaining(realm string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.m[realm]
	if st == nil {
		return 0
	}
	if d := time.Until(st.until); d > 0 {
		return d
	}
	return 0
}

// noteRestriction 记一次上游限制命中（server 侧唯一入口），返回 true = 调用方
// 必须**停手**（本请求不再换号）。
//
// 只处理 ErrAccountFault：账号级故障才与「封号」有关；限流/审核/请求级错误换号
// 仍是对的（那些不是账号的问题）。
func (h *Handler) noteRestriction(acct *auth.Auth, kind upstream.ErrKind, body string) bool {
	if acct == nil || kind != upstream.ErrAccountFault {
		return false
	}
	if !upstream.IsAccountRestriction(body) {
		return false
	}
	h.restrict.note(acct.Realm(), acct.UID, h.cfg.RestrictionHold, h.realmHealthy(acct.Realm()), h.cfg.RestrictionWaveDiv)
	// 无论是否触发连坐，本请求都必须停手：继续换号 = 继续往风控里送号。
	return true
}

// realmHealthy 返回该域此刻的可用号数（连坐阈值的基数）。Pool 为 nil（裸用/测试
// 的极简路径）时返回 0：阈值退化为下限 restrictionWaveMin，保护仍在。
func (h *Handler) realmHealthy(realm string) int {
	if h.cfg.Pool == nil {
		return 0
	}
	_, healthy, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(realm)
	return healthy
}
