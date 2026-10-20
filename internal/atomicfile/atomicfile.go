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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// stagingName 决定暂存文件名。默认是 tmpName（唯一名）。
//
// 抽成变量是为了让测试能观测「每次写用了哪个暂存名」——并发写的核心不变量
// 就是「不同写者不得共用同一个暂存名」。仅测试使用，生产路径恒为 tmpName。
var stagingName = tmpName

// Write 把 data 写入 path。
//
// mode 为**新建**文件时的权限位（已存在的文件保持原权限）。
// fallbackDirs 是同目录 tmp 不可写时依次尝试的暂存目录（可为 nil）。
// 返回 nil 表示 path 已包含 data 的全部内容。
//
// 失败时返回的错误里会带上可操作信息（哪个路径不可写、完整新内容留在哪）。
func Write(path string, data []byte, mode os.FileMode, fallbackDirs ...string) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	primary, nameErr := stagingName(dir, base)
	primaryErr := nameErr
	if nameErr == nil {
		primaryErr = os.WriteFile(primary, data, mode)
	}
	if primaryErr == nil {
		// 正常路径：同目录 tmp + rename（原子）。
		//
		// rename 可能因为**另一个写者正在替换同一目标**而失败 —— Windows 上
		// MoveFileEx(..., MOVEFILE_REPLACE_EXISTING) 在目标文件被其它句柄占用
		// 时返回 ERROR_ACCESS_DENIED(5) / ERROR_SHARING_VIOLATION(32)，
		// 这在「两个写者并发写同一文件」时是**必然会撞上**的（不是罕见故障）。
		// 此时退化为原地覆盖往往也会失败（目标正被另一个句柄独占），
		// 于是并发写会**偶发失败**（Windows CI 实测：60 轮里第 5 轮就挂）。
		//
		// 正确做法：这类「目标侧临时占用」的错误**重试几次**（每次间隔很短），
		// 让另一个写者先完成。重试仍失败才降级原地覆盖。
		renameErr := retryRename(primary, path)
		if renameErr == nil {
			return nil
		}
		// rename 失败（EXDEV 跨设备 / EBUSY 单文件挂载点 / 目标被占用…）：
		// 退化为原地覆盖；tmp 里已是完整新内容，失败时留在原地供恢复。
		//
		// 注意错误信息里**必须带上 rename 的原始原因**：否则像 Windows
		// 「源文件被独占」这类真实原因会被原地覆盖的次生错误盖住
		//（实测踩过：rename 因共享冲突失败 → 原地覆盖因目标尚不存在
		//  报 ERROR_FILE_NOT_FOUND → 排查时看到的是一个完全误导的错误）。
		if werr := writeInPlace(path, data); werr == nil {
			_ = os.Remove(primary)
			return nil
		} else {
			return fmt.Errorf("replace %s: rename 失败(%v)，原地覆盖也失败: %w（完整新内容保留在 %s）", path, renameErr, werr, primary)
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
		alt, err := stagingName(dir, base)
		if err != nil {
			continue
		}
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
			return fmt.Errorf("replace %s: 原地覆盖也失败: %w（完整新内容保留在 %s）", path, werr, alt)
		}
	}

	// 全部退路都失败：把原因讲清楚，并附上权限排查指引。
	msg := fmt.Sprintf("write %s: %v", path, primaryErr)
	if primary != "" {
		msg += fmt.Sprintf("（临时文件 %s 无法创建：所在目录不可写）", primary)
	}
	if errors.Is(primaryErr, fs.ErrPermission) {
		msg += "\n（Docker 部署：容器内用户对目标文件所在目录无写权限。解法任选：" +
			"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
			"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
			"3) compose 设 user: \"0:0\" 以 root 运行）"
	}
	return errors.New(msg)
}

// renameRetryAttempts / renameRetryDelay 是 rename 因「目标侧临时占用」而失败时的
// 重试次数与间隔。
//
// 为什么需要重试：Windows 上 MoveFileEx(..., MOVEFILE_REPLACE_EXISTING) 在目标
// 文件**正被另一个句柄打开**时返回 ERROR_ACCESS_DENIED(5) /
// ERROR_SHARING_VIOLATION(32)。两个写者并发替换同一目标时，这必然会撞上
// （不是罕见故障）——另一个写者的 rename 窗口只有几微秒，但重试机制是让
// 双方最终都成功的唯一办法：
//
//	写者 A: rename(tmpA, target)   ← 成功
//	写者 B: rename(tmpB, target)   ← 撞上 A 的窗口 → ERROR_ACCESS_DENIED
//
// 不重试时 B 的原地覆盖也会失败（目标正被 A 的句柄独占），结果就是
// 「一次本该成功的并发写变成失败」（Windows CI 实测：60 轮里第 5 轮挂）。
const (
	renameRetryAttempts = 40
	renameRetryDelay    = 5 * time.Millisecond
)

// retryRename 执行 rename，遇到「目标侧临时占用」类错误时短暂重试。
//
// 返回 nil 表示成功；否则返回**最后一次**的错误（供调用方决定是否降级）。
func retryRename(tmp, path string) error {
	var err error
	for i := 0; i < renameRetryAttempts; i++ {
		err = os.Rename(tmp, path)
		if err == nil {
			return nil
		}
		if !isTargetBusyErr(err) {
			return err // 跨设备 / 挂载点等，重试没有意义
		}
		time.Sleep(renameRetryDelay)
	}
	return err
}

// tmpName 为暂存文件生成一个**唯一**名字（放在 dir 下）。
// 为什么不能用固定的 path+".tmp"：两个并发写同一目标会互相踩——
//  1. 后写者的 os.WriteFile 会把前写者**已暂存好的内容截断重写**，
//     前写者随后 rename，落盘的是后写者的内容（甚至两者的混合）；
//  2. 前写者 rename 成功后暂存文件消失，后写者 rename 报「源文件不存在」；
//  3. Windows 上更糟：文件正被另一个进程打开时 MoveFileEx 会因共享冲突失败
//     （ERROR_SHARING_VIOLATION / ERROR_ACCESS_DENIED），
//     随后的原地覆盖又因「目标文件尚不存在」报 ERROR_FILE_NOT_FOUND
//     → 一次本该成功的并发写变成失败。
//
// 唯一名字让每个写者只操作自己的暂存文件，rename 仍然是同文件系统内的
// 原子替换，互不干扰。
//
// 代价：进程被强杀可能留下形如 config.json.tmp-<16位十六进制> 的残留文件。
// 这是「宁可留下可识别的残留，也不写坏主文件」的取舍。
func tmpName(dir, base string) (string, error) {
	var b [8]byte
	for attempt := 0; attempt < 4; attempt++ {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		name := filepath.Join(dir, base+".tmp-"+hex.EncodeToString(b[:]))
		if _, err := os.Lstat(name); os.IsNotExist(err) {
			return name, nil
		}
	}
	return "", fmt.Errorf("无法为 %s 在 %s 生成唯一暂存文件名", base, dir)
}

// writeInPlace 原地覆盖目标文件（不新建）。用于「rename 不可用」的降级路径：
// 单文件 bind mount（EBUSY）与跨设备（EXDEV）。
//
// 注意：目标文件**可能还不存在**（例如并发写同一个新文件时，
// 另一个写者已把目标 rename 走、或目标本来就是新建的）。
// Windows 上「以 O_WRONLY|O_TRUNC 打开一个不存在的文件」报
// ERROR_FILE_NOT_FOUND → 必须用 O_CREATE 补上，否则一次本该成功的写
// 会以「系统找不到指定的文件」失败。
func writeInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
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
//
// ⚠️ **这是 POSIX 口径**。Windows 上 rename 的失败原因是 ERROR_NOT_SAME_DEVICE
// （跨卷）与 ERROR_ACCESS_DENIED（目标被占用/只读），而 Windows 的
// syscall.EXDEV 是臆造占位值（0x20000000，永远匹配不上真实 errno）
// → 这个函数在 Windows 上**认不出**跨卷与占用两种情况。
// **跨平台的调用方请用包内的 CanFallbackToInPlace**
// （见 atomicfile_windows.go / atomicfile_other.go），它按平台补齐了错误码。
func IsReplaceableErr(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EBUSY)
}
