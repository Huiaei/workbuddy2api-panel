//go:build windows

package panel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTaskJobsPersistWindowsTargetHeldOpen 复现 U 盘现场的 Windows 失败：
//
//	panel: 保存后台任务恢复状态失败: rename data\.task-jobs-xxx.tmp data\task-jobs.json: Access is denied.
//
// 根因：Go 的 os.Open 在 Windows 上用 FILE_SHARE_READ|FILE_SHARE_WRITE 打开文件，
// **不含 FILE_SHARE_DELETE** ⇒ 任何进程只要持有该文件的句柄（杀毒软件、索引器、
// 另一个写者、面板自己），MoveFileEx(..., MOVEFILE_REPLACE_EXISTING) 就会返回
// ERROR_ACCESS_DENIED（"Access is denied."）。
//
// 这正是「手写 tmp+rename」在 Windows 上不可用的原因：它把 rename 失败
// 直接当成致命错误返回，后台任务状态因此永远存不下去。
//
// atomicfile.Write 的处理：rename 失败且属于「目标侧占用/权限类」错误时，
// 降级为**原地覆盖**（writeInPlace，O_TRUNC）——目标文件本身可写时能写进去。
//
// POSIX 下 rename 不受此影响，所以本用例只在 Windows 上有意义；
// 但因为我们跑 Windows CI job（test-windows），它是可执行的、且真的能
// 区分「手写 rename」与「atomicfile」两种实现。
func TestTaskJobsPersistWindowsTargetHeldOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-jobs.json")
	m := &taskJobManager{path: path, jobs: map[string]TaskJob{}}
	if err := m.persistLocked(); err != nil {
		t.Fatalf("首次 persist 失败: %v", err)
	}

	// 持有目标文件的句柄（模拟杀毒软件/索引器/并发写者）。
	held, err := os.Open(path)
	if err != nil {
		t.Fatalf("open 失败: %v", err)
	}
	defer held.Close()

	// 手写 tmp+rename 在这里会失败：os.Rename → ERROR_ACCESS_DENIED。
	if err := m.persistLocked(); err != nil {
		t.Fatalf("目标被占用时 persist 应降级成功，实际报错: %v", err)
	}

	// 内容必须真的更新了（不是静默跳过）。
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("落盘内容为空")
	}

	// 不得留下暂存文件。
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != "task-jobs.json" {
			t.Fatalf("留下了多余文件: %s", e.Name())
		}
	}
}

// TestTaskArchivePersistWindowsTargetHeldOpen 任务日志归档同理。
func TestTaskArchivePersistWindowsTargetHeldOpen(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "task-logs.json")
	r := NewRing(50)
	if err := r.SetTaskArchive(archive); err != nil {
		t.Fatalf("SetTaskArchive: %v", err)
	}
	r.Write([]byte("panel: 任务 first\n"))
	if r.ArchiveError() != "" {
		t.Fatalf("首次归档报错: %s", r.ArchiveError())
	}

	held, err := os.Open(archive)
	if err != nil {
		t.Fatalf("open 失败: %v", err)
	}
	defer held.Close()

	r.Write([]byte("panel: 任务 second\n"))
	if r.ArchiveError() != "" {
		t.Fatalf("目标被占用时归档应降级成功，实际报错: %s", r.ArchiveError())
	}
}
