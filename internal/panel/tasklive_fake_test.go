package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// liveFake 实时任务测试的假上游：按 X-User-Id 区分账号，记录每次请求的口径头、
// 每号请求次数与并发峰值。
type liveFake struct {
	calls   atomic.Int32
	mu      sync.Mutex
	mpCalls map[string]string // uid -> X-Client-Platform
	perUID  map[string]int    // uid -> 请求次数

	failUID string        // 这个号回 401（模拟 token 失效）
	doneUID string        // 这个号的任务全部已领
	block   chan struct{} // 非 nil 时请求阻塞到它被关闭
	delay   time.Duration // 每次请求的模拟耗时（并发测试用）

	inflight    atomic.Int32 // 当前在处理的请求数
	maxInflight atomic.Int32 // 观测到的并发峰值
}

func (f *liveFake) handler(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/tasks") {
		http.NotFound(w, r)
		return
	}
	f.calls.Add(1)
	uid := r.Header.Get("X-User-Id")

	// 并发峰值观测：先占坑再干活，defer 释放。并发闸门生效时峰值不得超过
	// taskLiveConcurrency（一轮 = 启用账号数，账号多时会远超这个值）。
	n := f.inflight.Add(1)
	for {
		cur := f.maxInflight.Load()
		if n <= cur || f.maxInflight.CompareAndSwap(cur, n) {
			break
		}
	}
	defer f.inflight.Add(-1)

	f.mu.Lock()
	if f.mpCalls == nil {
		f.mpCalls = map[string]string{}
		f.perUID = map[string]int{}
	}
	f.mpCalls[uid] = r.Header.Get("X-Client-Platform")
	f.perUID[uid]++
	f.mu.Unlock()

	if f.block != nil {
		<-f.block
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if uid == f.failUID {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"token expired"}`))
		return
	}
	pending := `{"task_code":"chat_5","title":"发起 5 次对话","target_count":5,"progress":{"current":2,"target":5},
			 "reward_credit":100,"accept_status":"accepted"},`
	if uid == f.doneUID {
		pending = ""
	}
	_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[` + pending + `
			{"task_code":"first_buddy","title":"领取一只 Buddy","target_count":1,"progress":{"current":1,"target":1},
			 "reward_credit":300,"accept_status":"claimed"}]}}`))
}

// newLivePanel 起假上游和一个带 cn-1 / cn-2 两个国内号的面板（global 域也指向假上游）。
func newLivePanel(t *testing.T, f *liveFake, extra ...*auth.Auth) *Panel {
	t.Helper()
	useRealmForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.HTTP, up.ChatBaseCN, up.ChatBaseGlobal = srv.Client(), srv.URL, srv.URL
	p := pool.New("")
	p.Add(testRealmAuth(t, "cn-1", "cn"))
	p.Add(testRealmAuth(t, "cn-2", "cn"))
	for _, a := range extra {
		p.Add(a)
	}
	return New(Config{Pool: p, Upstream: up})
}

// getLiveJSON 打一次实时任务端点并解出 JSON；非 200 直接失败。
func getLiveJSON(t *testing.T, p *Panel, url string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", url, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// liveRowOf 取某一账号的行（不存在返回 nil）。
func liveRowOf(out map[string]any, uid string) map[string]any {
	accts, _ := out["accounts"].([]any)
	for _, a := range accts {
		if m := a.(map[string]any); m["uid"] == uid {
			return m
		}
	}
	return nil
}
