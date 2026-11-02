//go:build windows && tray

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/energye/systray"
	"golang.org/x/sys/windows"
)

// trayIcon 托盘图标（.ico）。经 go:embed 打进二进制——便携版没有外部资源依赖，
// U 盘换机器也能直接跑。
//
//go:embed assets/tray.ico
var trayIcon []byte

// instanceMutexName 单实例互斥体名。Local\ 前缀 = 只在本机会话内互斥
// （多用户终端服务器场景下各会话互不干扰）。
const instanceMutexName = `Local\WorkBuddy2API-Panel-Singleton`

// singleInstance 单实例守卫。
type singleInstance struct {
	handle windows.Handle
}

// acquireSingleInstance 抢占单实例互斥体。
//
// 返回 duplicate=true 表示已有实例在跑（此时本次启动应当让路：打开面板后退出，
// 而不是再起一个进程抢同一个端口 —— 第二个实例会在 listen 时报 address in use）。
func acquireSingleInstance() (*singleInstance, bool, error) {
	name, err := windows.UTF16PtrFromString(instanceMutexName)
	if err != nil {
		return nil, false, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("create instance mutex: %w", err)
	}
	return &singleInstance{handle: handle}, false, nil
}

// Close 释放互斥体。
func (s *singleInstance) Close() error {
	if s == nil || s.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(s.handle)
	s.handle = 0
	return err
}

// openPanelInBrowser 用系统默认浏览器打开面板地址。
func openPanelInBrowser(rawURL string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(rawURL)
	if err != nil {
		return err
	}
	if err := windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("open %s: %w", rawURL, err)
	}
	return nil
}

// showFatalError 启动失败时弹窗（GUI 构建没有控制台，只能靠弹窗告知）。
func showFatalError(title, message string) {
	caption, errCaption := windows.UTF16PtrFromString(title)
	text, errText := windows.UTF16PtrFromString(message)
	if errCaption != nil || errText != nil {
		return
	}
	_, _ = windows.MessageBox(0, text, caption, windows.MB_OK|windows.MB_ICONERROR)
}

// runDesktop 启动系统托盘并阻塞到用户点「退出程序」。
//
// 生命周期：systray.Run 会建立消息循环并阻塞；onReady 里建菜单项；托盘退出后
// systray.Quit 解除阻塞，runDesktop 返回，调用方再做优雅停机。
//
// ctx 取消（Ctrl+C / 服务停止）也会触发 systray.Quit，避免进程赖着不走。
func runDesktop(ctx context.Context, onOpen func()) error {
	var ready atomic.Bool
	readyCh := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-readyCh:
				systray.Quit()
			case <-doneCh:
			}
		case <-doneCh:
		}
	}()

	systray.Run(func() {
		systray.SetIcon(trayIcon)
		systray.SetTooltip("WorkBuddy2API")

		openItem := systray.AddMenuItem("打开面板", "在默认浏览器中打开管理面板")
		openItem.Click(func() {
			if onOpen != nil {
				onOpen()
			}
		})
		logItem := systray.AddMenuItem("打开日志目录", "打开日志所在的文件夹")
		logItem.Click(func() {
			if dir, err := trayLogDir(); err == nil {
				if err := openPanelInBrowser("file:///" + filepath.ToSlash(dir)); err != nil {
					log.Printf("打开日志目录失败: %v", err)
				}
			}
		})
		quitItem := systray.AddMenuItem("退出程序", "停止网关并退出")
		quitItem.Click(func() {
			systray.Quit()
		})

		ready.Store(true)
		close(readyCh)
		showStartupNotice()
	}, func() {})

	close(doneCh)
	if !ready.Load() {
		return errors.New("系统托盘初始化失败")
	}
	log.Printf("系统托盘已就绪：右键托盘图标可打开面板或退出")
	return nil
}

// trayLogDir 托盘构建的日志目录（exe 同目录 logs/）。
func trayLogDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "logs"), nil
}
