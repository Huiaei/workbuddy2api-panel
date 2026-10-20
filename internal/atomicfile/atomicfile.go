// Package atomicfile 提供「优先原子替换、不可写时降级原地覆盖」的落盘原语。
//
// # 为什么需要它（issue #226 / #134）
//
// Docker 官方推荐的部署形态是把 config.json 以**单文件** bind mount 进容器
// （./config.json:/app/config.json）。此时「先写 path+".tmp" 再 rename」的原子替换
// 会两头撞墙：
//
//  1. tmp 落在**镜像内**的 /app 目录（属主 uid 10001），而 compose 允许以宿主机
//     uid 运行容器（PUID/PGID=1000）。→ tmp 写入 EACCES：
//     "write config: open /app/config.json.tmp: permission denied"。
//     面板「保存配置」因此永远失败，而 auths/ 与 data/ 挂载目录却是可写的——
//     两个官方解法（以本机 uid 运行 / chown 10001）恰好互相冲突。
//  2. 即便 tmp 写成功，rename 覆盖单文件挂载点会得到 EBUSY（device or resource busy）。
//
// 本包的处理：同目录 tmp 写不动时，依次退到调用方给的**可写暂存目录**
// （Docker 部署下即 ./data），再退到 os.TempDir()；暂存成功后优先 rename
// （同一文件系统时仍是原子替换），跨设备（EXDEV）或挂载点（EBUSY）时退化为对
// 目标文件的原地覆盖，并保留暂存文件里的完整新内容以便可手工恢复。
//
// 原地覆盖是有损的（进程在写一半时被杀 → 目标文件为空），因此它只是**最后手段**：
// 正常路径始终是「同目录 tmp + rename」，失败时的第一选择是「换目录暂存 + rename」。
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Write 把 data 写入 path。
//
// mode 为**新建**文件时的权限位（已存在的文件保持原权限）。
// fallbackDirs 是同目录 tmp 不可写时依次尝试的暂存目录（可为 nil）。
// 返回 nil 表示 path 已包含 data 的全部内容。
//
// 失败时返回的错误里会带上可操作信息（哪个路径不可写、完整新内容留在哪）。
func Write(path string, data []byte, mode os.FileMode, fallbackDirs ...string) error {
	primary := path + ".tmp"
	primaryErr := os.WriteFile(primary, data, mode)
	if primaryErr == nil {
		// 正常路径：同目录 tmp + rename（原子）。
		if err := os.Rename(primary, path); err == nil {
			return nil
		} else {
			// rename 失败（EXDEV 跨设备 / EBUSY 单文件挂载点 / EXDEV 目标为目录…）：
			// 退化为原地覆盖；tmp 里已是完整新内容，失败时留在原地供恢复。
			if werr := writeInPlace(path, data); werr == nil {
				_ = os.Remove(primary)
				return nil
			} else {
				return fmt.Errorf("replace %s: %w（完整新内容保留在 %s）", path, werr, primary)
			}
		}
	}

	// 同目录 tmp 不可写：退到可写暂存目录。
	// 常见现场：单文件 bind mount（tmp 落在镜像目录 /app）+ 非 10001 uid 运行。
	// 末位追加系统临时目录：几乎总能写（/tmp 默认 1777）。
	candidates := append(append([]string{}, fallbackDirs...), os.TempDir())
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		alt := filepath.Join(dir, filepath.Base(path)+".tmp")
		if err := os.WriteFile(alt, data, mode); err != nil {
			continue
		}
		if err := os.Rename(alt, path); err == nil {
			return nil
		}
		// 跨设备 / 挂载点：原地覆盖目标（目标文件本身通常是可写的）。
		if werr := writeInPlace(path, data); werr == nil {
			_ = os.Remove(alt)
			return nil
		} else {
			return fmt.Errorf("replace %s: %w（完整新内容保留在 %s）", path, werr, alt)
		}
	}

	// 全部退路都失败：把原因讲清楚，并附上权限排查指引。
	msg := fmt.Sprintf("write %s: %v（临时文件 %s 无法创建：所在目录不可写）", path, primaryErr, primary)
	if errors.Is(primaryErr, fs.ErrPermission) {
		msg += "\n（Docker 部署：容器内用户对目标文件所在目录无写权限。解法任选：" +
			"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
			"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
			"3) compose 设 user: \"0:0\" 以 root 运行）"
	}
	return errors.New(msg)
}

// writeInPlace 原地覆盖目标文件（不新建）。用于「rename 不可用」的降级路径：
// 单文件 bind mount（EBUSY）与跨设备（EXDEV）。
func writeInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// IsReplaceableErr 报告 rename 失败是否属于「应当降级为原地覆盖」的情形：
// EXDEV（跨设备）与 EBUSY（单文件 bind mount 挂载点）。
func IsReplaceableErr(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EBUSY)
}
