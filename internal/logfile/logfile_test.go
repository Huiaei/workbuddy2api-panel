package logfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewCreatesParentDirAndAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "gateway.log")
	w, err := New(path, 1, 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.Close()
	// 再开一次：应追加而非截断。
	w2, err := New(path, 1, 2)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()
	w2.Write([]byte("world\n"))
	w2.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "hello") || !strings.Contains(s, "world") {
		t.Fatalf("应为追加写，实际内容: %q", s)
	}
}

func TestWriteAppendsToExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	os.WriteFile(path, []byte("existing\n"), 0o644)
	w, err := New(path, 1, 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	w.Write([]byte("new\n"))
	w.Close()
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "existing") || !strings.Contains(string(b), "new") {
		t.Fatalf("既有内容被截断: %q", string(b))
	}
}

func TestRotateKeepsBackupsAndDropsOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rot.log")
	// maxSize 1MiB，但为了快测试，直接手工把 size 顶到上限。
	w, err := New(path, 1, 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	// 直接写满 1MiB 触发轮转。
	chunk := strings.Repeat("x", 1024*1024)
	if _, err := w.Write([]byte(chunk)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.mu.Lock()
	w.rotate()
	w.mu.Unlock()
	w.Close()
	// 当前文件应存在，.1 与 .2 也在（backups=2 ⇒ 最多 .1/.2）。
	for _, suffix := range []string{"", ".1", ".2"} {
		p := path + suffix
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("期望存在 %s: %v", p, err)
		}
	}
	// .3（超出保留份数）不应存在。
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatalf("超出保留份数的备份不该存在: %s", path+".3")
	}
}

func TestNewRejectsEmptyPath(t *testing.T) {
	if _, err := New("", 1, 1); err == nil {
		t.Fatal("空路径应报错")
	}
}

func TestNewOnUnwritableDirFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 上 os.MkdirAll(dir, 0o555) 不会真的让目录不可写
		//（Windows 的只读属性对目录不生效），New 会成功打开文件 ⇒ 本用例不成立。
		t.Skip("Windows 无 POSIX 目录权限语义，跳过")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 可绕过目录权限，跳过")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "ro")
	os.MkdirAll(sub, 0o555)
	defer os.Chmod(sub, 0o755)
	w, err := New(filepath.Join(sub, "x.log"), 1, 1)
	if err == nil {
		// 万一真的打开了，必须关掉，否则 TempDir 清理会因文件被占用而报错。
		w.Close()
		t.Fatal("不可写目录应报错")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, err := New(filepath.Join(dir, "c.log"), 1, 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("二次 Close 应无错: %v", err)
	}
}

func TestNilWriterIsNoop(t *testing.T) {
	var w *Writer
	n, err := w.Write([]byte("x"))
	if n != 1 || err != nil {
		t.Fatalf("nil writer 应是无害 no-op: n=%d err=%v", n, err)
	}
	if w.Path() != "" {
		t.Fatal("nil writer Path 应为空")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("nil writer Close 应无错: %v", err)
	}
}
