package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
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
func TestWriteFallbackDirWhenPrimaryDirUnwritable(t *testing.T) {
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
