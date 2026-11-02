package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestSafeWriterSwallowsErrors 保证「没有控制台的 GUI 进程」写 stderr 失败时
// 不会把整条 MultiWriter 链路掐断。
//
// 这是托盘版能落地的前提：io.MultiWriter 遇到第一个写错误就提前返回，
// 后面挂着的面板环形缓冲与日志文件就再也收不到内容了 —— 用户在面板里看到
// 的会是「一片空白」，而不是错误信息。
func TestSafeWriterSwallowsErrors(t *testing.T) {
	w := safeWriter{w: &failingWriter{}}
	payload := []byte("hello")
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("safeWriter.Write 返回错误: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("safeWriter.Write 写入长度 = %d, want %d", n, len(payload))
	}
}

// TestSafeWriterNilSink 不 panic（无控制台句柄时 os.Stderr 也可能是 nil）。
func TestSafeWriterNilSink(t *testing.T) {
	var w safeWriter
	payload := []byte("x")
	n, err := w.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("safeWriter.Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
}

// TestMultiWriterSurvivesBrokenConsole 端到端：第一个 sink 写失败时，
// 后面的 sink 仍要收到内容。
func TestMultiWriterSurvivesBrokenConsole(t *testing.T) {
	var buf bytes.Buffer
	combined := io.MultiWriter(safeWriter{w: &failingWriter{}}, &buf)
	if _, err := combined.Write([]byte("payload")); err != nil {
		t.Fatalf("写入返回错误: %v", err)
	}
	if buf.String() != "payload" {
		t.Fatalf("后续 sink 未收到内容: %q", buf.String())
	}
}

// TestLogSinksAttachPanel 面板环形缓冲应排在最前（面板可读性优先），
// 且文件 sink 存在时链路仍然完整。
func TestLogSinksAttachPanel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wb2api.log")
	s := newLogSinks()
	if err := s.attachFile(path, 1, 1); err != nil {
		t.Fatalf("attachFile: %v", err)
	}
	defer s.Close()

	var ring bytes.Buffer
	s.attachPanel(&ring)
	s.system.Write([]byte("hello"))
	if !bytes.Contains(ring.Bytes(), []byte("hello")) {
		t.Fatalf("面板环形缓冲未收到日志: %q", ring.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志文件: %v", err)
	}
	if !bytes.Contains(data, []byte("hello")) {
		t.Fatalf("日志文件未收到内容: %q", string(data))
	}
}

// TestLogSinksAttachFileFailure 落盘失败不阻断启动（返回错误但 sink 仍可用）。
func TestLogSinksAttachFileFailure(t *testing.T) {
	s := newLogSinks()
	// 用一个「普通文件」当父目录 ⇒ mkdir 必然失败。
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备阻塞文件: %v", err)
	}
	if err := s.attachFile(filepath.Join(blocker, "sub", "x.log"), 1, 1); err == nil {
		t.Fatal("落盘失败时应返回错误")
	}
	s.system.Write([]byte("still alive"))
}

// failingWriter 永远失败的 writer（模拟 GUI 进程里无效的控制台句柄）。
type failingWriter struct{}

func (f *failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("invalid handle")
}
