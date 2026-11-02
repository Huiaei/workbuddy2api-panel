//go:build windows && tray

package main

import (
	"log"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const (
	// startupNoticeFileName 标记文件：存在即表示用户勾了「以后不再显示」。
	startupNoticeFileName = ".startup-notice-disabled"
	// idYes MessageBox 的「是」按钮返回值（winuser.h IDYES=6）。
	// x/sys/windows 没有导出这个常量，这里按 Win32 定义写死。
	idYes = 6
)

// startupNoticeMarkerPath 标记文件路径（exe 同目录）。
func startupNoticeMarkerPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), startupNoticeFileName), nil
}

// startupNoticeSuppressed 标记文件存在即视为用户已选择不再显示。
func startupNoticeSuppressed(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// persistStartupNoticeChoice 记录「以后不再显示」。
func persistStartupNoticeChoice(path string, suppress bool) error {
	if !suppress {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

// showStartupNotice 首次启动提示。
//
// 为什么用 MessageBox 而不是 TaskDialog：TaskDialog 需要程序清单声明
// comctl32 v6（common-controls v6），而清单要编译进 .syso 资源；无 windres 的
// 构建环境里没法生成这个资源，TaskDialogIndirect 会直接失败降级。MessageBox
// 是系统原生、永远可用的，代价只是没有复选框——用「是/否」两个按钮表达同样的
// 意思（选「否」= 以后还要提示）。
//
// 提示本身不可省：托盘版没有控制台窗口，首次启动时用户若不知道「右键托盘图标」
// 就完全无从下手。
func showStartupNotice() {
	marker, err := startupNoticeMarkerPath()
	if err != nil {
		log.Printf("启动提示: 无法确定标记文件路径: %v", err)
		return
	}
	if startupNoticeSuppressed(marker) {
		return
	}

	suppress := askStartupNotice()
	if !suppress {
		return
	}
	if err := persistStartupNoticeChoice(marker, true); err != nil {
		log.Printf("启动提示: 保存「不再显示」设置失败: %v", err)
		return
	}
	log.Printf("启动提示: 已创建 %s，后续启动不再显示", marker)
}

// askStartupNotice 弹提示框，返回用户是否选择「以后不再显示」。
func askStartupNotice() bool {
	text, errText := windows.UTF16PtrFromString(
		"WorkBuddy2API 已在系统托盘中运行。\n\n" +
			"右键点击托盘图标，选择「打开面板」即可进入管理界面。\n\n" +
			"是否以后不再显示此提示？")
	title, errTitle := windows.UTF16PtrFromString("WorkBuddy2API 已启动")
	if errText != nil || errTitle != nil {
		return false
	}
	// MB_YESNO：选「是」= 以后不再显示；选「否」= 以后还要提示。
	ret, _ := windows.MessageBox(0, text, title, windows.MB_YESNO|windows.MB_ICONINFORMATION|windows.MB_SETFOREGROUND)
	return ret == idYes
}
