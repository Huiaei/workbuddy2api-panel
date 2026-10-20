package atomicfile

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestWriteConcurrentSameTarget 多写者并发替换**同一**目标时，每次写都必须成功，
// 且落盘内容必须是某一个写者的**完整**内容。
//
// Windows CI 实测过这个坑：MoveFileEx(..., MOVEFILE_REPLACE_EXISTING) 在目标
// 文件正被另一个句柄打开时返回 ERROR_ACCESS_DENIED(5) /
// ERROR_SHARING_VIOLATION(32) ⇒ 并发写**偶发失败**（60 轮里第 5 轮就挂）。
//
// 不重试时：rename 撞上另一个写者的窗口 → 失败 → 原地覆盖也失败（目标正被
// 独占）⇒ 一次本该成功的写变成失败。修法是 retryRename 对「目标侧临时占用」
// 短暂重试。
//
// 非 Windows 平台上 POSIX rename(2) 不会因目标被打开而失败，所以这个用例在
// Linux 上是回归基线；在 Windows 上是真实回归。
func TestWriteConcurrentSameTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// 先落一个基线文件（后续 rename 覆盖的是已存在的目标）。
	if err := Write(path, []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}

	const rounds = 40
	for i := 0; i < rounds; i++ {
		payload := []byte("payload-" + string(rune('A'+i%26)) + "-0123456789")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); errs[0] = Write(path, payload, 0o600) }()
		go func() { defer wg.Done(); errs[1] = Write(path, payload, 0o600) }()
		wg.Wait()
		for j, err := range errs {
			if err != nil {
				t.Fatalf("round %d writer %d: 并发写失败: %v", i, j, err)
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("round %d: 读回失败: %v", i, err)
		}
		if string(raw) != string(payload) {
			t.Fatalf("round %d: 落盘内容被写坏: %q want %q", i, string(raw), string(payload))
		}
	}
}

// TestWriteConcurrentNoStagingLeftover 并发写结束后不得残留暂存文件
// （暂存文件里有明文 token）。
func TestWriteConcurrentNoStagingLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := Write(path, []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = Write(path, []byte("payload"), 0o600)
		}(i)
	}
	wg.Wait()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("目录里应只剩目标文件，实际=%v", names)
	}
}
