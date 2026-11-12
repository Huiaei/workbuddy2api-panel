package panel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newEnterpriseTaskPanel 造一个「企业版账号」的测试面板：账号带 EnterpriseID，
// 上游端点被计数（任何真实请求都会留下痕迹）。
func newEnterpriseTaskPanel(t *testing.T, uid string, calls *int) *Panel {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
	}))
	t.Cleanup(server.Close)

	accounts := pool.New("")
	accounts.Add(&auth.Auth{UID: uid, Nickname: "enterprise", AccessToken: "token", EnterpriseID: "ent-123"})

	client := upstream.New()
	client.HTTP = &http.Client{Timeout: 5 * time.Second}
	client.ChatBaseCN, client.BillingBaseCN, client.WebBaseCN = server.URL, server.URL, server.URL

	p := New(Config{
		Pool:             accounts,
		Upstream:         client,
		AutoTasksEnabled: func() bool { return true },
	})
	appCtx, cancel := context.WithCancel(context.Background())
	if err := p.StartTaskJobs(appCtx, t.TempDir()+"/task-jobs.json"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		p.StopTaskJobs()
	})
	return p
}

// 企业版账号不排成长任务作业：作业入口直接 422，且不产生任何上游请求。
func TestEnterpriseAccountDoesNotStartGrowthTaskJob(t *testing.T) {
	calls := 0
	p := newEnterpriseTaskPanel(t, "ent-task", &calls)

	if _, _, err := p.StartAccountTaskJob("ent-task", "daily"); !errors.Is(err, errTaskJobNoGrowth) {
		t.Fatalf("StartAccountTaskJob error = %v, want errTaskJobNoGrowth", err)
	}
	if jobs := p.ListTaskJobs(); len(jobs) != 0 {
		t.Fatalf("enterprise account created jobs: %+v", jobs)
	}
	if calls != 0 {
		t.Fatalf("enterprise account made %d upstream requests, want 0", calls)
	}

	// 新账号自动任务同样不得排队。
	p.StartNewAccountTasks("ent-task")
	if jobs := p.ListTaskJobs(); len(jobs) != 0 {
		t.Fatalf("enterprise new-account task created jobs: %+v", jobs)
	}
	if calls != 0 {
		t.Fatalf("enterprise new-account task made %d upstream requests, want 0", calls)
	}

	// HTTP 入口返回 422 + 说明文案。
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/ent-task/tasks/auto_all", nil)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if calls != 0 {
		t.Fatalf("enterprise HTTP entry made %d upstream requests, want 0", calls)
	}
}

// 每日排程遇到企业版账号时静默跳过，不刷错误日志，也不产生作业。
func TestGrowthQueueSkipsEnterpriseAccount(t *testing.T) {
	calls := 0
	p := newEnterpriseTaskPanel(t, "ent-queue", &calls)
	p.RunGrowthQueueOnce()
	if jobs := p.ListTaskJobs(); len(jobs) != 0 {
		t.Fatalf("daily queue created jobs for enterprise account: %+v", jobs)
	}
	if calls != 0 {
		t.Fatalf("daily queue made %d upstream requests, want 0", calls)
	}
}
