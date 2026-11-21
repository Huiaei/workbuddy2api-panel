package panel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// autoTaskListState 记录假上游收到的列表请求，并可按需返回指定任务清单。
type autoTaskListState struct {
	mu        sync.Mutex
	listCalls int
	mpCalls   int
	failList  bool
	tasksJSON string
	accepts   int
}

func (s *autoTaskListState) counters() (list, mp int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls, s.mpCalls
}

// newAutoTaskListPanel 造一个只回答成长任务列表/接受接口的假上游。
// 默认口径返回 st.tasksJSON，小程序口径恒返回空列表（除非 mpJSON 单独设置）。
func newAutoTaskListPanel(t *testing.T, st *autoTaskListState) (*Panel, *auth.Auth) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/accept") || strings.HasSuffix(r.URL.Path, "/accept-batch") {
			st.mu.Lock()
			st.accepts++
			st.mu.Unlock()
			fmt.Fprint(w, `{"code":0,"msg":"OK","data":{}}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks") {
			http.NotFound(w, r)
			return
		}
		mp := r.Header.Get("X-Client-Platform") == "miniprogram"
		st.mu.Lock()
		if mp {
			st.mpCalls++
		} else {
			st.listCalls++
		}
		fail, js := st.failList, st.tasksJSON
		st.mu.Unlock()
		if fail {
			http.Error(w, "upstream list failed", http.StatusBadGateway)
			return
		}
		if mp || js == "" {
			js = "[]"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"tasks":%s}}`, js)
	}))
	t.Cleanup(server.Close)
	oldGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldGap })
	client := upstream.New()
	client.HTTP = &http.Client{Timeout: 5 * time.Second}
	client.ChatBaseCN, client.WebBaseCN, client.BillingBaseCN = server.URL, server.URL, server.URL
	p := New(Config{Upstream: client})
	a := &auth.Auth{UID: "autotask-batch", AccessToken: "token"}
	return p, a
}

// 列表接口不可用时，一轮只该报 1 条错误，而不是把 25 个配置项逐个放大成失败。
func TestRunAutoAllReportsSingleErrorWhenTaskListUnavailable(t *testing.T) {
	st := &autoTaskListState{failList: true}
	p, a := newAutoTaskListPanel(t, st)

	results := p.runAutoAll(a)
	if len(results) != 1 {
		t.Fatalf("results = %d 项，想要 1 项（一次系统性失败只报一条）: %v", len(results), results)
	}
	if code := fmt.Sprint(results[0]["task_code"]); code != "(任务列表)" {
		t.Fatalf("task_code = %q，想要 (任务列表)", code)
	}
	if status := fmt.Sprint(results[0]["status"]); status != "error" {
		t.Fatalf("status = %q，想要 error", status)
	}
	msg := fmt.Sprint(results[0]["message"])
	if !strings.Contains(msg, "本次未执行任何任务") {
		t.Fatalf("message 必须说清本轮没跑任何任务: %q", msg)
	}
	list, mp := st.counters()
	if list != 2 {
		t.Fatalf("列表接口应重试一次后放弃（2 次），实际 %d 次", list)
	}
	if mp != 0 {
		t.Fatalf("列表都拿不到时不该再去拉小程序口径（实际 %d 次）", mp)
	}
}

// 上游已下架的配置任务不再逐项跑（旧实现会跑成 25 项全跳过），且清单只拉一次。
func TestRunAutoAllRunsOnlyTasksPresentUpstream(t *testing.T) {
	st := &autoTaskListState{tasksJSON: `[{"task_code":"Library_read","title":"体验资料库","target":1,"current":1,"claimed":true,"accept_status":"claimed"},{"task_code":"RichMeow_Chat","title":"桌面端对话","target":1,"current":1,"claimed":true,"accept_status":"claimed"}]`}
	p, a := newAutoTaskListPanel(t, st)

	results := p.runAutoAll(a)
	if len(results) != 2 {
		t.Fatalf("results = %d 项，想要 2 项（只跑上游现存的两个任务）: %v", len(results), results)
	}
	for _, r := range results {
		code := fmt.Sprint(r["task_code"])
		if code != "Library_read" && code != "RichMeow_Chat" {
			t.Fatalf("出现了上游已不存在的任务项 %q: %v", code, results)
		}
		if status := fmt.Sprint(r["status"]); status != "done" {
			t.Fatalf("%s status = %q，已领取任务应为 done", code, status)
		}
	}
	list, mp := st.counters()
	if list != 1 {
		t.Fatalf("默认口径清单应只拉 1 次（旧实现每个任务拉一次），实际 %d 次", list)
	}
	if mp != 1 {
		t.Fatalf("小程序口径清单应拉 1 次，实际 %d 次", mp)
	}
	if st.accepts != 0 {
		t.Fatalf("已领取任务不该再走批量接受，实际 %d 次", st.accepts)
	}
}

// 上游任务清空时给一条明确说明，而不是 25 条「该账号没有此任务」。
func TestRunAutoAllReportsNoAutoTasksWhenListEmpty(t *testing.T) {
	st := &autoTaskListState{}
	p, a := newAutoTaskListPanel(t, st)

	results := p.runAutoAll(a)
	if len(results) != 1 {
		t.Fatalf("results = %d 项，想要 1 项: %v", len(results), results)
	}
	if code := fmt.Sprint(results[0]["task_code"]); code != "(可自动任务)" {
		t.Fatalf("task_code = %q，想要 (可自动任务)", code)
	}
	if status := fmt.Sprint(results[0]["status"]); status != "skipped" {
		t.Fatalf("status = %q，想要 skipped", status)
	}
	if list, _ := st.counters(); list != 1 {
		t.Fatalf("列表只该拉 1 次，实际 %d 次", list)
	}
}

// 收尾文案必须如实反映失败项，不能一律写「全部可自动任务已处理」。
func TestTaskJobSummaryMessageReflectsFailures(t *testing.T) {
	m := &taskJobManager{jobs: map[string]TaskJob{
		"u-fail": {Results: []map[string]any{
			{"status": "error"}, {"status": "error"}, {"status": "done"}, {"status": "skipped"},
		}},
		"u-skip": {Results: []map[string]any{{"status": "skipped"}, {"status": "skipped"}}},
		"u-done": {Results: []map[string]any{{"status": "done"}}},
	}}

	got := m.taskJobSummaryMessage("u-fail", "全部可自动任务已处理")
	if !strings.Contains(got, "2 项失败") || strings.Contains(got, "全部可自动任务已处理") {
		t.Fatalf("失败轮次文案 = %q，必须报失败项数且不能说全部已处理", got)
	}
	if !strings.Contains(got, "成功 1 项") || !strings.Contains(got, "跳过 1 项") {
		t.Fatalf("失败轮次文案 = %q，应同时给出成功与跳过项数", got)
	}
	if got := m.taskJobSummaryMessage("u-skip", "全部可自动任务已处理"); !strings.Contains(got, "上游当前没有可自动执行的任务") {
		t.Fatalf("全跳过文案 = %q，应说明上游无任务", got)
	}
	if got := m.taskJobSummaryMessage("u-done", "全部可自动任务已处理"); got != "全部可自动任务已处理" {
		t.Fatalf("成功轮次文案 = %q，应保持原文案", got)
	}
	if got := m.taskJobSummaryMessage("missing", "全部可自动任务已处理"); got != "全部可自动任务已处理" {
		t.Fatalf("未知账号文案 = %q，应回落原文案", got)
	}
}
