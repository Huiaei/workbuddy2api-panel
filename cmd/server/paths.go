package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// exeDir 返回当前可执行文件所在目录；取不到时返回空串。
//
// 为什么需要它：Windows 上双击 exe 启动时，进程工作目录**不一定**是 exe 所在
// 目录（从快捷方式启动时甚至可能是 C:\Windows\System32）。所有相对路径
// （config.json / auths/ / data/）都必须锚在 exe 目录上，否则会出现
// 「换个方式启动就找不到账号」这种玄学问题。
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// resolveConfigPath 决定要读哪个配置文件。
//
// explicit = 用户显式给了 -config：按原样使用（相对路径相对当前工作目录，
// 由调用方保证；这是命令行用户的明确意图，不该被悄悄改写）。
//
// 未显式给时（双击 / 托盘启动 / 直接跑 exe）：
//   - 托盘构建优先用 exe 同目录的 config.json —— 便携部署的语义是
//     「exe 旁边那一套文件就是我的全部」，用户拷到 U 盘就该直接能跑；
//   - 控制台构建优先用当前目录的 config.json —— 保持开发者「在项目目录里
//     go run ./cmd/server」的既有行为不变；
//   - 候选都不存在时，返回首选路径（让后续自动生成推荐配置写在那里）。
func resolveConfigPath(flagVal string, explicit bool) string {
	if explicit && flagVal != "" {
		return flagVal
	}
	if flagVal != "" {
		// 给了 -config 但没显式标记（理论上不会发生，防御性处理）。
		return flagVal
	}

	cwd, _ := os.Getwd()
	cwdCfg := filepath.Join(cwd, "config.json")
	exeCfg := filepath.Join(exeDir(), "config.json")

	if trayBuild {
		// 托盘版：exe 同目录优先。
		if fileExists(exeCfg) {
			return exeCfg
		}
		if fileExists(cwdCfg) {
			return cwdCfg
		}
		if exeDir() != "" {
			return exeCfg
		}
		return cwdCfg
	}

	// 控制台版：当前目录优先。
	if fileExists(cwdCfg) {
		return cwdCfg
	}
	if fileExists(exeCfg) {
		return exeCfg
	}
	return cwdCfg
}

// fileExists 判断路径是否指向一个存在的普通文件。
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// anchorWorkingDir 把工作目录切到配置文件所在目录。
//
// 为什么用 chdir 而不是把配置里的相对路径逐个改写成绝对路径：
//   - 配置文件里的 ./auths、./data/state.json 这类相对路径是**用户可读的**，
//     改写成绝对路径后，面板保存配置会把绝对路径写回文件，拷到别的机器就废了；
//   - chdir 之后，所有相对路径天然锚在「配置所在目录」，配置文件内容保持可移植；
//   - Docker 部署（-config /app/config.json，WORKDIR /app）行为不变。
//
// 切不切得动都不影响启动失败：chdir 失败只记一条 WARN，继续用原工作目录
// （相对路径仍能工作，只是锚点不同）。
func anchorWorkingDir(configPath string) {
	if configPath == "" {
		return
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return
	}
	dir := filepath.Dir(abs)
	if dir == "" {
		return
	}
	if err := os.Chdir(dir); err != nil {
		log.Printf("WARN: 无法切换工作目录到 %s（相对路径将相对原工作目录解析）: %v", dir, err)
		return
	}
	log.Printf("工作目录已锚定到配置所在目录: %s", dir)
}

// describeConfigPath 生成给用户看的配置路径说明（写进日志）。
func describeConfigPath(path string) string {
	if path == "" {
		return "（默认配置 + 环境变量）"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return fmt.Sprintf("%s", abs)
}
