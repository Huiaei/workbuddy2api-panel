package main

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfile"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// logSinks 描述「系统日志」与「chat 流水日志」各自的基础输出目标。
//
// 为什么要有这一层：托盘构建是 GUI 子系统（-H=windowsgui），进程没有可用的
// 控制台句柄。此时写 os.Stderr / os.Stdout 会返回「句柄无效」错误——更糟的是
// io.MultiWriter 遇到第一个写失败就**提前中断**，后面的 sink（面板环形缓冲、
// 日志文件）就再也收不到内容了。
//
// 所以控制台输出统一包一层 safeWriter（吞掉写错误），文件 sink 排在 MultiWriter
// 末位：托盘构建即使没有控制台，日志也一定落到文件与面板。
type logSinks struct {
	system io.Writer
	chat   io.Writer
	closer io.Closer
	// path 文件 sink 的实际路径（空 = 没落盘）。
	path string
}

// safeWriter 吞掉底层写错误。
//
// 存在的唯一理由：让「没有控制台的 GUI 进程」不会因为写 stderr 失败而把整个
// MultiWriter 链路掐断（面板与日志文件因此收不到任何内容）。
type safeWriter struct{ w io.Writer }

func (s safeWriter) Write(p []byte) (int, error) {
	if s.w == nil {
		return len(p), nil
	}
	n, _ := s.w.Write(p)
	if n < len(p) {
		// 报告写入长度不足但不给错误，避免 MultiWriter 中断。
		return len(p), nil
	}
	return n, nil
}

// newLogSinks 初始化基础 sink（此时还没有配置文件，先只挂控制台）。
//
// 文件 sink 在读到配置后由 attachFile 接入；托盘构建若配置里没写 log_file，
// 会回落到 exe 同目录 logs/wb2api.log —— 托盘程序没有控制台，不落盘就等于
// 「出问题时完全看不到任何信息」。
func newLogSinks() *logSinks {
	return &logSinks{
		system: safeWriter{w: os.Stderr},
		chat:   safeWriter{w: os.Stdout},
	}
}

// attachFile 追加文件 sink（写失败不阻断启动）。
//
// 路径为空时：托盘构建回落到默认路径；控制台构建保持不落盘（与现有行为一致）。
func (s *logSinks) attachFile(path string, maxSizeMB, backups int) error {
	if path == "" {
		path = defaultLogFilePath()
	}
	if path == "" {
		return nil
	}
	file, err := logfile.New(path, maxSizeMB, backups)
	if err != nil {
		return err
	}
	s.closer = file
	s.path = path
	// 文件 sink 放在最后：即便它写失败，也不会挡住面板环形缓冲。
	s.system = io.MultiWriter(s.system, file)
	s.chat = io.MultiWriter(s.chat, file)
	return nil
}

// init 把系统日志接到基础 sink 上（面板环形缓冲在稍后 attachPanel 接入）。
func (s *logSinks) init() {
	log.SetOutput(s.system)
}

// attachPanel 把面板环形缓冲插到最前面：面板可读性优先，且环形缓冲写失败
// 不会阻断后面的文件/控制台。
func (s *logSinks) attachPanel(ring io.Writer) {
	system := s.system
	chat := s.chat
	if ring != nil {
		system = io.MultiWriter(ring, system)
		chat = io.MultiWriter(ring, chat)
	}
	s.system = system
	s.chat = chat
	log.SetOutput(system)
	server.SetChatLogOutput(chat)
}

// Close 关闭文件 sink。
func (s *logSinks) Close() error {
	if s == nil || s.closer == nil {
		return nil
	}
	return s.closer.Close()
}

// defaultLogFilePath 托盘构建的默认日志路径（exe 同目录 logs/wb2api.log）；
// 控制台构建返回空串（不擅自落盘，行为与现在一致）。
func defaultLogFilePath() string {
	if !trayBuild {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "logs", "wb2api.log")
}
