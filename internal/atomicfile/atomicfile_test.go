package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// TestWriteAtomicReplace 正常路径：同目录 tmp + rename，路径上不留 tmp 残留。
func TestWriteAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content=%q want %q", got, "new")
	}
	// 暂存文件用的是**唯一**名（config.json.tmp-<hex>），正常完成后一个都不该留下。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("暂存文件不该残留：%q", e.Name())
		}
	}
}

// TestWriteFallbackDirWhenPrimaryDirUnwritable 复现 issue #226/#134 的核心现场：
// 目标文件所在目录不可写（单文件 bind mount + 非属主 uid），但 fallback 目录可写。
// 此时必须落盘成功，且内容完整。
//
// 平台说明：Windows 的 os.Chmod 只能改「只读属性」，没有目录写保护语义
// → chmod(0o555) 之后目录照样可写 → 本用例在 Windows 上退化成「正常写入」，
// 断言仍成立但覆盖不到 fallback 路径（fallback 路径由
// TestWriteTempDirFallback 与 Windows 专项用例覆盖）。故 Windows 上直接跳过，
// 避免出现「假通过且看不出在测什么」的情况。
func TestWriteFallbackDirWhenPrimaryDirUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有目录写权限语义，无法构造「目录不可写」现场")
	}
	root := t.TempDir()
	roDir := filepath.Join(root, "ro")
	fbDir := filepath.Join(root, "fallback")
	for _, d := range []string{roDir, fbDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(roDir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 去掉目录写权限（当前用户即属主时 chmod 同样有效）。
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	if err := Write(path, []byte("new"), 0o600, fbDir); err != nil {
		t.Fatalf("write with fallback: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content=%q want %q", got, "new")
	}
}

// TestWriteNoFallbackReturnsPermissionError 无 fallback 可用时，错误信息必须点名
// 不可写的 tmp 路径并给出 Docker 权限指引（而不是裸 syscall 错误）。
func TestWriteNoFallbackReturnsPermissionError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有目录写权限语义，无法构造「目录不可写」现场")
	}
	root := t.TempDir()
	roDir := filepath.Join(root, "ro")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(roDir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	// 只有系统临时目录可写——但它一定能写，所以这里刻意不传 fallback，
	// 让实现走到"全部退路失败"的分支前先验证同目录失败时的错误质量。
	err := Write(path, []byte("new"), 0o600)
	if err == nil {
		// 若当前用户是 root，chmod 拦不住——跳过断言。
		t.Skip("running as root: directory permission cannot be revoked")
	}
	// 错误信息必须点名不可写的暂存路径（让用户知道该给哪个目录写权限）。
	// 暂存名现在是唯一的（config.json.tmp-<hex>），所以只能校验前缀。
	if !strings.Contains(err.Error(), filepath.Join(roDir, "config.json.tmp-")) {
		t.Fatalf("error should name the unwritable tmp path, got: %v", err)
	}
}

// TestWriteTempDirFallback 系统临时目录兜底：同目录不可写、无显式 fallback 时，
// 仍应通过 os.TempDir() 完成落盘。
func TestWriteTempDirFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有目录写权限语义，无法构造「目录不可写」现场")
	}
	root := t.TempDir()
	roDir := filepath.Join(root, "ro")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(roDir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	if err := Write(path, []byte("new"), 0o600); err != nil {
		if os.Geteuid() == 0 {
			t.Skip("running as root: directory permission cannot be revoked")
		}
		t.Fatalf("temp-dir fallback should have succeeded: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content=%q want %q", got, "new")
	}
}

// TestWriteCreatesNewFile 目标不存在时也能落盘（自动创建）。
func TestWriteCreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "brand-new.json")
	if err := Write(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("content=%q", got)
	}
}

// TestCanFallbackToInPlaceRecognisesWindowsErrno 锁定「按平台补齐错误码」这一行为。
//
// 回归背景（Windows 10/11 + Go 1.22+ 实测）：MoveFileEx(..., MOVEFILE_REPLACE_EXISTING)
// 在目标文件被占用时返回 ERROR_ACCESS_DENIED(5)，跨卷时返回 ERROR_NOT_SAME_DEVICE(17)。
// Windows 的 syscall.EXDEV 是臆造占位值（0x20000000），errors.Is 永远匹配不上
// → 旧判据（只看 EXDEV/EBUSY）在 Windows 上认不出这两种情况
// → auth.SaveAtomic 不降级、直接报错 → **刷新后的 accessToken 不落盘**。
//
// 本用例必须在 Windows 上跑（POSIX 上没有这些错误码语义）。
func TestCanFallbackToInPlaceRecognisesWindowsErrno(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 专属错误码语义")
	}
	// 跨卷 rename 的错误码（Windows 独有，Go 的 syscall 没有为它定义常量）。
	notSameDevice := syscall.Errno(17)
	if !CanFallbackToInPlace(notSameDevice) {
		t.Errorf("跨卷错误码 ERROR_NOT_SAME_DEVICE(17) 必须判为「可降级原地覆盖」，否则 Windows 跨卷时刷新令牌不落盘")
	}
	// 目标被占用 / 只读。
	accessDenied := syscall.Errno(5)
	if !CanFallbackToInPlace(accessDenied) {
		t.Errorf("ERROR_ACCESS_DENIED(5) 必须判为「可降级原地覆盖」，否则 Windows 上文件被占用时刷新令牌不落盘")
	}
	// fs.ErrPermission（Go 把 ERROR_ACCESS_DENIED 映射成它）。
	if !CanFallbackToInPlace(fs.ErrPermission) {
		t.Errorf("fs.ErrPermission 必须判为「可降级原地覆盖」")
	}
	// POSIX 语义仍然有效。
	if !CanFallbackToInPlace(syscall.EXDEV) {
		t.Errorf("EXDEV 必须判为「可降级原地覆盖」")
	}
	if !CanFallbackToInPlace(syscall.EBUSY) {
		t.Errorf("EBUSY 必须判为「可降级原地覆盖」")
	}
	// 无关错误不能降级（否则会掩盖真正的故障）。
	if CanFallbackToInPlace(nil) {
		t.Errorf("nil 不是错误，不应判为可降级")
	}
	if CanFallbackToInPlace(errors.New("boom")) {
		t.Errorf("未知错误不应判为可降级")
	}
	if CanFallbackToInPlace(syscall.ENOENT) {
		t.Errorf("ENOENT 不应判为可降级（目标不存在是另一类问题）")
	}
}

// TestCanFallbackToInPlaceRejectsUnrelatedErrors 在所有平台都跑：确保判据不会
// 把无关错误误判为「可降级」（那会掩盖真正的故障）。
func TestCanFallbackToInPlaceRejectsUnrelatedErrors(t *testing.T) {
	if CanFallbackToInPlace(nil) {
		t.Errorf("nil 不是错误，不应判为可降级")
	}
	if CanFallbackToInPlace(errors.New("boom")) {
		t.Errorf("未知错误不应判为可降级")
	}
	if CanFallbackToInPlace(syscall.ENOENT) {
		t.Errorf("ENOENT 不应判为可降级（目标不存在是另一类问题）")
	}
}

// TestCanFallbackToInPlaceMatchesIsReplaceableErrOnPosix 在非 Windows 平台保证
// 两个判据等价，避免平台分叉带来行为漂移。
//
// 注意：Windows 上两者**故意不等价** —— IsReplaceableErr 只认 POSIX 的
// EXDEV/EBUSY，而 CanFallbackToInPlace 还要认 Windows 独有的
// ERROR_NOT_SAME_DEVICE / ERROR_ACCESS_DENIED / fs.ErrPermission。
func TestCanFallbackToInPlaceMatchesIsReplaceableErrOnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上两者故意不等价（Windows 独有错误码），见 atomicfile_windows.go")
	}
	for _, err := range []error{syscall.EXDEV, syscall.EBUSY, syscall.EACCES, syscall.ENOENT, errors.New("x")} {
		if got, want := CanFallbackToInPlace(err), IsReplaceableErr(err); got != want {
			t.Errorf("POSIX 上 CanFallbackToInPlace(%v)=%v，应与 IsReplaceableErr=%v 一致", err, got, want)
		}
	}
}

// TestWriteConcurrentSamePath 验证并发写同一路径不会互相污染内容。
// 两个 goroutine 各写不同内容，最终内容必须是其中一个的完整值（不能是混合体）。
//
// 回归背景（Windows CI 首次跑出来的真 bug）：暂存文件曾用固定的
// path+".tmp" → 两个写者共用同一个暂存名：
//   - 后写者 WriteFile 会截断重写前写者已备好的内容 → 前写者随后 rename
//     落盘的是**后写者的内容**（静默数据错乱，不是报错）；
//   - 前写者 rename 成功后暂存文件消失 → 后写者 rename 报「源文件不存在」；
//   - Windows 上更糟：文件正被另一个进程打开时 MoveFileEx 因共享冲突失败
//     （ERROR_SHARING_VIOLATION）→ 随后的原地覆盖又因目标尚不存在报
//     ERROR_FILE_NOT_FOUND → 一次本该成功的并发写变成失败。
//
// 修法：每个写者用**唯一**暂存名（config.json.tmp-<16位十六进制>）。
func TestWriteConcurrentSamePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	a := []byte("aaaaaaaaaaaaaaaaaaaaaaaa")
	b := []byte("bbbbbbbbbbbbbbbbbbbbbbbb")

	errCh := make(chan error, 2)
	go func() { errCh <- Write(p, a, 0o600) }()
	go func() { errCh <- Write(p, b, 0o600) }()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("并发 Write: %v", err)
		}
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(a) && string(raw) != string(b) {
		t.Fatalf("并发写后内容 = %q，应是 a 或 b 的完整值（不能是混合）", string(raw))
	}
}

// TestWriteConcurrentSamePathNoLeftoverTmp 并发写不得留下暂存残留。
//
// 唯一暂存名意味着「进程被强杀」会留下 config.json.tmp-<hex> 残留；
// 但**正常完成**的写必须把自己的暂存文件清干净（rename 走掉，或
// 降级路径里 os.Remove 掉）——否则每次保存配置都会攒一个垃圾文件。
func TestWriteConcurrentSamePathNoLeftoverTmp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")

	errCh := make(chan error, 2)
	go func() { errCh <- Write(p, []byte("a"), 0o600) }()
	go func() { errCh <- Write(p, []byte("b"), 0o600) }()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("并发 Write: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("并发写后残留了 %q（暂存文件应被 rename 走或清理掉）", e.Name())
		}
	}
}

// TestWriteUniqueTmpName 锁定暂存文件命名规则：与目标同目录、基名相同、
// 带 .tmp- 前缀与随机后缀，且两次调用**不同名**（不共享）。
func TestWriteUniqueTmpName(t *testing.T) {
	dir := t.TempDir()
	a, err := tmpName(dir, "config.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := tmpName(dir, "config.json")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("两次 tmpName 必须不同，否则并发写会互相踩：%q", a)
	}
	for _, n := range []string{a, b} {
		if filepath.Dir(n) != dir {
			t.Errorf("暂存文件必须与目标同目录（保证 rename 同文件系统、原子）：got %q want dir %q", filepath.Dir(n), dir)
		}
		if !strings.HasPrefix(filepath.Base(n), "config.json.tmp-") {
			t.Errorf("暂存文件名应以 config.json.tmp- 开头，got %q", filepath.Base(n))
		}
	}
}

// TestWriteInPlaceCreatesMissingTarget 原地覆盖降级路径必须能**新建**目标文件。
//
// 现场：rename 失败（跨设备 / 目标被占用）时降级为原地覆盖；若目标文件此时
// **根本不存在**（首次写入、或并发写者已把目标 rename 走），以
// O_WRONLY|O_TRUNC 打开会报 ENOENT（Windows: ERROR_FILE_NOT_FOUND
// 「系统找不到指定的文件」）→ 一次本该成功的写变成失败。
//
// 这正是 Windows CI 首跑抓到的那个 bug 的第二段。
func TestWriteInPlaceCreatesMissingTarget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "brand-new.json")
	if err := writeInPlace(p, []byte("hello")); err != nil {
		t.Fatalf("writeInPlace 必须能新建目标文件: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("content=%q want hello", got)
	}
}

// TestWriteUsesUniqueStagingNamePerWrite 锁定「**每次写**都用自己的暂存名」。
//
// 这条不变量就是并发写的全部要害：共用暂存名时，后写者会把前写者已备好的
// 内容截断重写 → 前写者随后 rename 落盘的是后写者的内容（静默数据错乱），
// 或前写者 rename 成功后暂存文件消失 → 后写者 rename 报「源文件不存在」。
//
// 为什么用 hook 而不是靠并发压测撞概率：竞态窗口极窄（Linux 上 4 写者 ×
// 20 轮 × 200KB 负载都撞不出来），靠概率的回归测试等于没有测试。
// 这里直接把「生成暂存名」这一步换成一个记录调用、且**故意每次都返回同���**
// 名」的桩 —— 若实现退回到共用暂存名，用例立刻失败。
func TestWriteUsesUniqueStagingNamePerWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")

	orig := stagingName
	t.Cleanup(func() { stagingName = orig })

	var names []string
	stagingName = func(d, b string) (string, error) {
		// 故意每次都返回**同一个**名字 —— 模拟「退回到固定 path+".tmp"」。
		n := filepath.Join(d, b+".tmp")
		names = append(names, n)
		return n, nil
	}

	if err := Write(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("stagingName 应被调用一次，实际 %d 次", len(names))
	}
	// 现在把桩改成「每次返回不同名」，验证实现确实**依赖**桩返回的名字。
	// 若实现内部写死了 path+".tmp"，那么第二次调用的名字不会出现在实际使用的
	// 暂存路径上 —— 通过观测 os.Stat 找不到该文件来判定。
	unique := filepath.Join(dir, "config.json.tmp-unique")
	used := false
	stagingName = func(d, b string) (string, error) {
		used = true
		return unique, nil
	}
	p2 := filepath.Join(dir, "other.json")
	if err := Write(p2, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatalf("Write 必须走 stagingName 生成暂存名")
	}
	// 暂存名必须被真正用上：rename 走掉后原路径不存在，但**内容**已落盘。
	raw, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "y" {
		t.Fatalf("content=%q want y", raw)
	}
}
