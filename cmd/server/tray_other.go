//go:build !windows || !tray

package main

import (
	"context"
	"errors"
)

// runDesktop 非托盘构建：没有桌面集成，等 ctx 取消即可。
//
// 语义与 Windows 托盘版一致 —— 阻塞到退出信号，避免调用方另写一套等待逻辑。
func runDesktop(ctx context.Context, _ func()) error {
	<-ctx.Done()
	return nil
}

// singleInstance 非 Windows/非托盘构建的单实例守卫（空实现）。
type singleInstance struct{}

// acquireSingleInstance 非 Windows/非托盘构建：不做单实例守卫（容器与 Linux
// 部署下多实例是正常用法，端口冲突由 listen 报错暴露）。
func acquireSingleInstance() (*singleInstance, bool, error) {
	return &singleInstance{}, false, nil
}

// Close 无操作。
func (s *singleInstance) Close() error { return nil }

// openPanelInBrowser 非 Windows 构建：无法拉起系统浏览器。
func openPanelInBrowser(string) error {
	return errors.New("打开浏览器仅在 Windows 托盘版可用")
}

// showFatalError 非 Windows 构建：无弹窗，错误只进日志。
func showFatalError(string, string) {}
