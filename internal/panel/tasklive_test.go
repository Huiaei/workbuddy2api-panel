package panel

// tasklive_test.go 钉住「实时任务」端点（GET /panel/api/tasks/live）的行为：
//   - 正常返回：每号一行、任务清单与三类计数、ttl_sec；
//   - TTL 内复用缓存（轮询不该把上游当靶子打），fresh=1 有下限、过了才穿透；
//   - 并发上限：一轮的账号并发不超过 taskLiveConcurrency；
//   - 单账号失败只标在该行，不拖垮其它账号，也不重试；
//   - 国际号只走默认口径（不带小程序头），国内号走小程序口径；
//   - 全部领完的号不带任务表（计数仍在）；
//   - 慢轮次不阻塞请求、不叠第二轮；
//   - 走 withAuth（未带 key 一律 401）。
//
// 前六条都是「看板会不会变成压测 / 一个号挂了会不会整块空掉 / 口径混用」的直接判据。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestTasksLiveCountsAndCache 正常返回的字段与计数，以及 TTL 内不重复打上游。
func TestTasksLiveCountsAndCache(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f)

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if out["ok"] != true {
		t.Fatalf("ok 字段缺失: %+v", out)
	}
	if out["ttl_sec"].(float64) != 60 {
		t.Fatalf("ttl_sec=%v want 60", out["ttl_sec"])
	}
	accts, _ := out["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("accounts=%d want 2", len(accts))
	}
	first := liveRowOf(out, "cn-1")
	if first == nil {
		t.Fatalf("缺 cn-1 行: %+v", accts)
	}
	if first["realm"] != "cn" || first["nickname"] != "cn-1" {
		t.Fatalf("行字段不对: %+v", first)
	}
	// 假上游：chat_5 = accepted（未完成）→ pending；first_buddy = claimed → claimed。
	if first["pending"].(float64) != 1 || first["claimable"].(float64) != 0 || first["claimed"].(float64) != 1 {
		t.Fatalf("计数不对: %+v", first)
	}
	tasks, _ := first["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("还有待办的号应带完整任务表，实际 %d 条: %+v", len(tasks), first)
	}
	if n := f.calls.Load(); n != 2 { // 两个账号各一次
		t.Fatalf("首轮上游调用=%d want 2", n)
	}

	// 第二轮：TTL 内直接复用，不再打上游（删掉缓存这条立刻变红）。
	// 一轮是后台 goroutine，上游请求要过一会儿才发出去：给一小段宽限再数，
	// 否则「TTL 判断失效 + 起了一轮」只会表现为后台多打一次，断言看不见。
	out2 := getLiveJSON(t, p, "/panel/api/tasks/live")
	if out2["cached"] != true {
		t.Fatalf("TTL 内应命中缓存: %+v", out2)
	}
	time.Sleep(150 * time.Millisecond)
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("缓存命中仍打了上游：2 -> %d", n)
	}
	// 再读一次也一样：TTL 内反复轮询（多标签页）不该累积上游请求。
	out3 := getLiveJSON(t, p, "/panel/api/tasks/live")
	time.Sleep(150 * time.Millisecond)
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("反复轮询仍在打上游：2 -> %d", n)
	}
	if out3["age_sec"] == nil {
		t.Fatalf("age_sec 字段缺失: %+v", out3)
	}
}

// TestTasksLiveFreshHasFloor fresh=1 有下限：上一轮刚结束时反复点「立即刷新」仍回缓存，
// 过了下限才穿透。
func TestTasksLiveFreshHasFloor(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f)
	getLiveJSON(t, p, "/panel/api/tasks/live")
	for i := 0; i < 3; i++ {
		if out := getLiveJSON(t, p, "/panel/api/tasks/live?fresh=1"); out["cached"] != true {
			t.Fatalf("下限内的 fresh=1 应回缓存: %+v", out)
		}
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("下限内的 fresh=1 不该打上游：calls=%d want 2", n)
	}

	old := taskLiveFreshFloor
	taskLiveFreshFloor = time.Nanosecond
	t.Cleanup(func() { taskLiveFreshFloor = old })
	time.Sleep(2 * time.Millisecond)
	if out := getLiveJSON(t, p, "/panel/api/tasks/live?fresh=1"); out["cached"] != false {
		t.Fatalf("过了下限 fresh=1 应穿透: %+v", out)
	}
	if n := f.calls.Load(); n != 4 {
		t.Fatalf("calls=%d want 4", n)
	}
}

// TestTasksLiveConcurrencyCap 一轮的账号并发不超过 taskLiveConcurrency：
// 账号数远大于上限时应看到并行（峰值 > 1）但不超过上限（删掉信号量立刻变红）。
func TestTasksLiveConcurrencyCap(t *testing.T) {
	const extra = 7 // 加上 cn-1/cn-2 共 9 个启用账号
	f := &liveFake{delay: 20 * time.Millisecond}
	extras := make([]*auth.Auth, 0, extra)
	for i := 0; i < extra; i++ {
		extras = append(extras, testRealmAuth(t, fmt.Sprintf("cn-extra-%d", i), "cn"))
	}
	p := newLivePanel(t, f, extras...)

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if accts, _ := out["accounts"].([]any); len(accts) != 2+extra {
		t.Fatalf("accounts=%d want %d", len(accts), 2+extra)
	}
	if n := f.calls.Load(); n != int32(2+extra) {
		t.Fatalf("每个启用账号恰好一次请求：calls=%d want %d", n, 2+extra)
	}
	peak := f.maxInflight.Load()
	if peak > int32(taskLiveConcurrency) {
		t.Fatalf("并发峰值=%d 超过上限 %d", peak, taskLiveConcurrency)
	}
	if peak < 2 {
		t.Fatalf("并发峰值=%d，账号 9 个却完全串行（信号量可能退化成互斥）", peak)
	}
}

// TestTasksLivePerAccountFailureIsolated 一个号 401 只标在自己那行：其它号照常返回，
// 且失败号不重试（换口径重试会在封号潮里把一轮请求数翻倍）。让失败号拖垮整轮 → 变红。
func TestTasksLivePerAccountFailureIsolated(t *testing.T) {
	f := &liveFake{failUID: "cn-2"}
	p := newLivePanel(t, f)

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	accts, _ := out["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("一个号失败不该让整块空掉：accounts=%d", len(accts))
	}
	okRow, badRow := liveRowOf(out, "cn-1"), liveRowOf(out, "cn-2")
	if okRow == nil || okRow["error"] != nil {
		t.Fatalf("健康号不该有 error: %+v", okRow)
	}
	if badRow == nil || badRow["error"] == nil || badRow["error"] == "" {
		t.Fatalf("失败号应带 error: %+v", badRow)
	}
	if ts, ok := badRow["tasks"].([]any); ok && len(ts) != 0 {
		t.Fatalf("失败号不该有任务: %+v", badRow)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("每个号每轮恰好一次请求：calls=%d want 2", n)
	}
}

// TestTasksLiveGlobalAccountUsesDefaultOnly 国际号只走默认口径（不带小程序头）：
// 该头对国际账号没有验证过的契约，单号任务弹窗同样不探 MP。国内号仍走小程序口径。
func TestTasksLiveGlobalAccountUsesDefaultOnly(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f, testRealmAuth(t, "gl-1", "global"))

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if row := liveRowOf(out, "gl-1"); row == nil || row["realm"] != "global" {
		t.Fatalf("国际号应出现在结果里：%+v", out["accounts"])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.mpCalls["gl-1"]; got != "" {
		t.Fatalf("国际号不该带小程序口径头，实际 X-Client-Platform=%q", got)
	}
	if got := f.mpCalls["cn-1"]; got != "miniprogram" {
		t.Fatalf("国内号应走小程序口径，实际 X-Client-Platform=%q", got)
	}
	if n := f.perUID["gl-1"]; n != 1 {
		t.Fatalf("国际号应恰好一次默认口径请求，实际 %d 次（不该两个口径都打）", n)
	}
}

// TestTasksLiveSkipsDisabledAccounts 禁用号不查、不出现在结果里。
func TestTasksLiveSkipsDisabledAccounts(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f)
	p.cfg.Pool.Disable("cn-1", "test")

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if accts, _ := out["accounts"].([]any); len(accts) != 1 {
		t.Fatalf("禁用号不该出现在实时任务里：%d", len(accts))
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("禁用号不该打上游：calls=%d want 1", n)
	}
}

// TestTasksLiveFullyClaimedOmitsTasks 全部领完的号不带任务表（生产里占绝大多数），
// 计数仍要有，汇总才对得上。
func TestTasksLiveFullyClaimedOmitsTasks(t *testing.T) {
	f := &liveFake{doneUID: "cn-2"}
	p := newLivePanel(t, f)

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	done, busy := liveRowOf(out, "cn-2"), liveRowOf(out, "cn-1")
	if done == nil || done["claimed"].(float64) != 1 || done["pending"].(float64) != 0 {
		t.Fatalf("已领完的号计数应仍在: %+v", done)
	}
	if _, has := done["tasks"]; has {
		t.Fatalf("已领完的号不该带任务表: %+v", done)
	}
	if ts, _ := busy["tasks"].([]any); len(ts) != 2 {
		t.Fatalf("还有待办的号应带完整任务表: %+v", busy)
	}
}

// TestTasksLiveSlowRoundDoesNotBlockOrStack 一轮跑得比请求愿意等的久：请求在上限内带着
// refreshing=true 返回（上一份快照），后到的请求不能再叠一轮；上游放行后下一次拿到完整快照。
func TestTasksLiveSlowRoundDoesNotBlockOrStack(t *testing.T) {
	old, oldFloor := taskLiveWait, taskLiveFreshFloor
	taskLiveWait, taskLiveFreshFloor = 50*time.Millisecond, 0
	t.Cleanup(func() { taskLiveWait, taskLiveFreshFloor = old, oldFloor })

	f := &liveFake{block: make(chan struct{})}
	p := newLivePanel(t, f)
	// 断言失败时也要放行被阻塞的假上游，否则 httptest.Server.Close 会一直等；
	// 在 newLivePanel 之后注册，保证先于它的 Close 执行（Cleanup 后进先出）。
	var once sync.Once
	release := func() { once.Do(func() { close(f.block) }) }
	t.Cleanup(release)

	start := time.Now()
	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("请求被慢轮次拖住了：%v", elapsed)
	}
	if out["refreshing"] != true {
		t.Fatalf("一轮未完成时应报 refreshing：%+v", out)
	}
	if out["cached"] != true {
		t.Fatalf("等待超时后回的是上一份快照，cached 应为 true：%+v", out)
	}
	getLiveJSON(t, p, "/panel/api/tasks/live")
	getLiveJSON(t, p, "/panel/api/tasks/live?fresh=1")

	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out = getLiveJSON(t, p, "/panel/api/tasks/live")
		if out["refreshing"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("放行后一轮仍未结束：%+v", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if accts, _ := out["accounts"].([]any); len(accts) != 2 {
		t.Fatalf("放行后应拿到完整快照：%+v", out)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("慢轮次期间不该叠第二轮：calls=%d want 2", n)
	}
}

// TestTasksLiveRequiresAuth 新路由必须走 withAuth：api_key 非空时未带 Bearer 一律 401，
// 带了才放行（漏挂 withAuth → 匿名可打，这条立刻变红）。
func TestTasksLiveRequiresAuth(t *testing.T) {
	f := &liveFake{}
	useRealmForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.HTTP, up.ChatBaseCN, up.ChatBaseGlobal = srv.Client(), srv.URL, srv.URL
	pl := pool.New("")
	pl.Add(testRealmAuth(t, "cn-1", "cn"))
	panel := New(Config{Pool: pl, Upstream: up, APIKey: "test-key"})

	rec := httptest.NewRecorder()
	panel.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/tasks/live", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带 key 应 401，实际 %d", rec.Code)
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("401 不该触达上游：calls=%d", n)
	}

	req := httptest.NewRequest(http.MethodGet, "/panel/api/tasks/live", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	panel.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("带 key 应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}
