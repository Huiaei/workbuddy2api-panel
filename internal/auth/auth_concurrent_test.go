package auth

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestSaveAtomicConcurrentDistinctInstances 两个**不同**的 *Auth 实例持有同一个
// FilePath 时并发保存，不得出现「返回成功但内容没落盘」。
//
// 现场（issue：token 刷新不落盘）：SaveAtomic 之前固定用 FilePath+".tmp" 作为
// 暂存名。两个实例并发写时：
//   - A 写完 tmp → B 也写同一个 tmp（覆盖）→ A rename 走 → B rename 报 ENOENT；
//   - 或 A rename 走 → B rename 时目标已存在（Windows 报占用），内容是 A 的。
//
// 实测 300 轮 × 2 写者：SaveAtomic 返回 nil 但内容未落盘 **282 次**（被对方
// rename 走），另有 **266 次**报「源文件不存在」的假失败。
//
// a.mu 只保护**同一个对象**；scheduler 的 keepalive 刷新（scheduler.go）与
// handler 的按需刷新（handler.go）持有的是**不同**对象 ⇒ 互不排斥。
//
// 修法：暂存名唯一（同一目录下由目标路径派生 + 随机后缀），并复用
// internal/atomicfile 的原子替换路径。
func TestSaveAtomicConcurrentDistinctInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-concurrent.json")

	// 基准文件先存在（模拟磁盘上已有的账号文件）。
	seed := &Auth{UID: "u1", AccessToken: "seed", ExpiresAt: 100, FilePath: path}
	if err := seed.SaveAtomic(); err != nil {
		t.Fatal(err)
	}

	const rounds = 60
	for i := 0; i < rounds; i++ {
		// 每次用**不同**的 *Auth 对象（不同 mu），指向同一 FilePath。
		a := &Auth{UID: "u1", AccessToken: "token-A", ExpiresAt: int64(200 + i), FilePath: path}
		b := &Auth{UID: "u1", AccessToken: "token-B", ExpiresAt: int64(300 + i), FilePath: path}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); errs[0] = a.SaveAtomic() }()
		go func() { defer wg.Done(); errs[1] = b.SaveAtomic() }()
		wg.Wait()

		// 两次保存都不得**虚假失败**（ENOENT 那种）。
		for j, err := range errs {
			if err != nil {
				t.Fatalf("round %d writer %d: 虚假失败: %v", i, j, err)
			}
		}
		// 落盘内容必须是**某一个**写者的完整内容（不能是半截/空/别人的混合体）。
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("round %d: 读回失败: %v", i, err)
		}
		got, err := Parse(raw)
		if err != nil {
			t.Fatalf("round %d: 落盘内容不是合法 JSON（半截文件？）: %v\n内容=%q", i, err, string(raw))
		}
		if got.AccessToken != "token-A" && got.AccessToken != "token-B" {
			t.Fatalf("round %d: 落盘内容既不是 A 也不是 B: %q", i, got.AccessToken)
		}
		if got.UID != "u1" {
			t.Fatalf("round %d: UID 被写坏: %q", i, got.UID)
		}
	}
}

// TestSaveAtomicLeavesNoStagingFiles 保存后不得残留暂存文件。
//
// 残留的 .tmp 里有**明文 token**；虽然 auths 目录的 glob 扫不到它（不会当配置
// 加载），但它会一直躺在磁盘上。
func TestSaveAtomicLeavesNoStagingFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-staging.json")
	a := &Auth{UID: "u1", AccessToken: "t1", ExpiresAt: 100, FilePath: path}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "workbuddy-staging.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("目录里应只剩目标文件，实际=%v", names)
	}
}
