//go:build !windows || !tray

package main

// showStartupNotice 非托盘构建无启动提示（控制台里已经看得到启动日志）。
func showStartupNotice() {}
