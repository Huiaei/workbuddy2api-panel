//go:build !tray

package main

// trayBuild 标识当前二进制是否含系统托盘（tray build tag）。
//
// 为什么要分两种构建：
//   - 默认构建（无 tag）：控制台程序，日志往 stderr/stdout 走，适合服务器/容器；
//   - tray 构建（-tags tray，Windows 上再配 -H=windowsgui）：无控制台窗口的
//     桌面程序，日志落 exe 同目录 logs/ 下，托盘图标常驻。
//
// 两种构建共享同一份网关逻辑，差异只在「怎么启动、怎么停、日志去哪」。
const trayBuild = false
