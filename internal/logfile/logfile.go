// Package logfile 提供网关运行日志的落盘写入器：带体积上限的轮转追加写。
//
// 为什么需要：网关的运行日志（启动信息、上游错误、账号池事件）此前只往
// stderr/stdout 写。Windows 上双击或托盘启动时没有控制台窗口，出问题只能
// 靠猜；落盘后可以直接翻日志文件定位。
//
// 设计要点：
//   - 追加写，不截断历史（同一天多次重启的日志连成一条时间线）。
//   - 体积上限 + 轮转：超过上限后把当前文件改名成 .1，.1 变 .2 …，超过
//     保留份数的最老的被删除。避免日志无限增长把 U 盘/系统盘写满。
//   - 写失败不 panic：落盘是辅助能力，磁盘满/权限不足时不能让网关起不来，
//     降级为只打一行警告并继续（stderr 仍然有输出）。
//   - 并发安全：多个 goroutine（server/panel/scheduler）都会写。
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Writer 是轮转追加写文件。
type Writer struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	backups int
	f       *os.File
	size    int64
	written int64
}

// New 创建写入器并打开（追加模式）目标文件。
//
// maxSize <= 0 时用 64MiB，backups <= 0 时用 3。父目录不存在会被创建。
// 打不开文件时返回错误——调用方决定是否降级。
func New(path string, maxSizeMB int, backups int) (*Writer, error) {
	if path == "" {
		return nil, fmt.Errorf("logfile: empty path")
	}
	if maxSizeMB <= 0 {
		maxSizeMB = 64
	}
	if backups <= 0 {
		backups = 3
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("logfile: mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logfile: open %s: %w", path, err)
	}
	w := &Writer{
		path:    path,
		maxSize: int64(maxSizeMB) * 1024 * 1024,
		backups: backups,
		f:       f,
	}
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
	return w, nil
}

// Write 写入一段日志（自动补换行）。
func (w *Writer) Write(p []byte) (int, error) {
	if w == nil {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(p), nil
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	w.written += int64(n)
	if err == nil && w.size >= w.maxSize {
		// 轮转失败不致命：文件继续写，只是可能超限。
		w.rotate()
	}
	return n, err
}

// rotate 把当前文件轮转为 .1（.1→.2…），并重新打开一个新的当前文件。
func (w *Writer) rotate() {
	// 先关掉旧句柄，否则 Windows 上改名会失败（文件被占用）。
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	// 丢弃最老的备份。
	oldest := w.path + "." + strconv.Itoa(w.backups)
	if err := os.Remove(oldest); err == nil {
		_ = err
	}
	// 其余依次后移（从大到小，避免覆盖）。
	for i := w.backups - 1; i >= 1; i-- {
		src := w.path + "." + strconv.Itoa(i)
		dst := w.path + "." + strconv.Itoa(i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	// 当前文件 → .1
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		// 改名失败（权限/占用）：重新打开原文件继续写。
		if f, err2 := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err2 == nil {
			w.f = f
			if st, err3 := f.Stat(); err3 == nil {
				w.size = st.Size()
			}
		}
		return
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		w.f = nil
		return
	}
	w.f = f
	w.size = 0
}

// Close 关闭文件句柄。
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Path 返回日志文件路径。
func (w *Writer) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}
