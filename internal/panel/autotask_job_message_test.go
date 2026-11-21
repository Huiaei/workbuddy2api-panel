package panel

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 后台任务收尾文案必须走 taskJobSummaryMessage（端到端钉住调用点，而不只是函数本身）。
func TestTaskJobFinishMessageReflectsFailedBatch(t *testing.T) {
	st := &autoTaskListState{failList: true}
	p, a := newAutoTaskListPanel(t, st)
	accounts := pool.New("")
	accounts.Add(a)
	p.cfg.Pool = accounts

	ctx, cancel := context.WithCancel(context.Background())
	path := filepath.Join(t.TempDir(), "task-jobs.json")
	if err := p.StartTaskJobs(ctx, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		p.StopTaskJobs()
	})
	if _, _, err := p.StartAccountTaskJob(a.UID, "manual"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, job := range p.ListTaskJobs() {
			if job.UID != a.UID || job.Status != "finished" {
				continue
			}
			if strings.Contains(job.Message, "全部可自动任务已处理") {
				t.Fatalf("有失败项却报「全部已处理」: %q", job.Message)
			}
			if !strings.Contains(job.Message, "1 项失败") {
				t.Fatalf("收尾文案 = %q，应如实报 1 项失败", job.Message)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("后台任务未在 15s 内结束")
}
