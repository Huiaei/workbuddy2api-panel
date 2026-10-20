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
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file should not remain")
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
	if !strings.Contains(err.Error(), path+".tmp") {
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
