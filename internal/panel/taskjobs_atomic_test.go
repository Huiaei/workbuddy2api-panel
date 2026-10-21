package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTaskJobsPersistUsesAtomicWrite 后台任务状态必须走 atomicfile：
// 手写 tmp+rename 在 Windows 上替换被占用的目标会返回 "Access is denied."，
// 任务状态因此存不下去。
func TestTaskJobsPersistUsesAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-jobs.json")
	m := &taskJobManager{path: path, jobs: map[string]TaskJob{}}
	if err := m.persistLocked(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("状态文件未落盘: %v", err)
	}
	// 不得留下暂存文件。
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("留下了暂存文件: %s", e.Name())
		}
	}
	// 反复写不应失败（Windows 上 rename 撞占用是偶发的）。
	for i := 0; i < 30; i++ {
		if err := m.persistLocked(); err != nil {
			t.Fatalf("第 %d 次 persist 失败: %v", i, err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f taskJobFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
}

// TestTaskArchivePersistUsesAtomicWrite 任务日志归档同理。
func TestTaskArchivePersistUsesAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "task-logs.json")
	r := NewRing(50)
	if err := r.SetTaskArchive(archive); err != nil {
		t.Fatalf("SetTaskArchive: %v", err)
	}
	// 只有 taskPrefixes 里的行首才会进任务频道并触发归档。
	r.Write([]byte("panel: 任务 hello\n"))
	r.Write([]byte("| #001 | chat row\n"))
	for i := 0; i < 30; i++ {
		r.Write([]byte("panel: 任务 tick\n"))
	}
	if r.ArchiveError() != "" {
		t.Fatalf("归档报错: %s", r.ArchiveError())
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("归档文件未落盘: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("留下了暂存文件: %s", e.Name())
		}
	}
}

// TestTaskJobsPersistOnWindowsLockedTarget 目标被独占打开时仍应写成功
// （Windows 语义：MoveFileEx 替换被占用的目标返回 ERROR_ACCESS_DENIED）。
// POSIX 下 rename 不受此影响，所以该用例只在 Windows 上有意义，
// 但在其它平台也必须保证「不报错、不留暂存文件」。
func TestTaskJobsPersistOnWindowsLockedTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-jobs.json")
	m := &taskJobManager{path: path, jobs: map[string]TaskJob{}}
	if err := m.persistLocked(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if runtime.GOOS == "windows" {
		// 独占打开目标，模拟另一个写者/读者占着它。
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			t.Skipf("无法独占打开: %v", err)
		}
		defer f.Close()
	}
	if err := m.persistLocked(); err != nil {
		t.Fatalf("目标被占用时 persist 应降级成功，实际报错: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("留下了暂存文件: %s", e.Name())
		}
	}
}
