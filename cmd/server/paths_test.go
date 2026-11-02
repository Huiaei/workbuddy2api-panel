package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveConfigPathExplicitWins 显式 -config 不该被改写。
func TestResolveConfigPathExplicitWins(t *testing.T) {
	got := resolveConfigPath("D:/custom/cfg.json", true)
	if got != "D:/custom/cfg.json" {
		t.Fatalf("显式 -config 被改写: %q", got)
	}
}

// TestResolveConfigPathPrefersExistingFile 候选里存在的那份要被选中。
func TestResolveConfigPathPrefersExistingFile(t *testing.T) {
	dir := t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)

	custom := filepath.Join(dir, "config.json")
	if err := os.WriteFile(custom, []byte("{}"), 0o644); err != nil {
		t.Fatalf("准备配置: %v", err)
	}
	got := resolveConfigPath("", false)
	if got != filepath.Join(cwd, "config.json") {
		t.Fatalf("应选中当前目录已存在的配置，实际 %q", got)
	}
}

// TestResolveConfigPathEmptyValue 传空字符串时也要给出一个可用路径。
func TestResolveConfigPathEmptyValue(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	got := resolveConfigPath("", false)
	if got == "" {
		t.Fatal("不应返回空路径")
	}
	if !filepath.IsAbs(got) && got != filepath.Join(cwd, "config.json") {
		t.Fatalf("路径可疑: %q", got)
	}
}

// TestAnchorWorkingDir 相对路径锚定：切到配置所在目录后，./auths 之类的
// 相对路径才会落在配置旁边 —— 双击 exe 时工作目录不一定是 exe 目录。
func TestAnchorWorkingDir(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o644); err != nil {
		t.Fatalf("准备配置: %v", err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	anchorWorkingDir(cfg)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// 比较解析后的绝对路径（macOS/Linux 上 /tmp 可能是符号链接）。
	wantDir, _ := filepath.EvalSymlinks(dir)
	gotDir, _ := filepath.EvalSymlinks(wd)
	if gotDir != wantDir {
		t.Fatalf("工作目录 = %q, want %q", gotDir, wantDir)
	}
}

// TestAnchorWorkingDirMissingDir 目录不存在时不能 panic（只是不切换）。
func TestAnchorWorkingDirMissingDir(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	anchorWorkingDir(filepath.Join(t.TempDir(), "nope", "config.json"))
	wd, _ := os.Getwd()
	want, _ := filepath.EvalSymlinks(orig)
	got, _ := filepath.EvalSymlinks(wd)
	if got != want {
		t.Fatalf("目录不存在时不应切换工作目录: %q", got)
	}
}
