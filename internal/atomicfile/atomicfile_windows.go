//go:build windows

package atomicfile

import (
	"errors"
	"io/fs"
	"syscall"
)

// Windows 上的 rename 相关错误码（直接给字面量，避免把 golang.org/x/sys/windows
// 从间接依赖提成直接依赖）。
const (
	winErrNotSameDevice    syscall.Errno = 17 // ERROR_NOT_SAME_DEVICE：跨卷 rename
	winErrSharingViolation syscall.Errno = 32 // ERROR_SHARING_VIOLATION：目标被独占打开
	winErrLockViolation    syscall.Errno = 33 // ERROR_LOCK_VIOLATION：目标被锁
)

// windowsReplaceableErrno 列出「rename 失败后仍可以降级为原地覆盖」的 Windows 错误码。
//
// 为什么需要单独一份（Windows 10/11 + Go 1.22+ 实测结论）：
//
//   - MoveFileEx(..., MOVEFILE_REPLACE_EXISTING) 在**目标文件被其它句柄占用**时
//     返回 ERROR_ACCESS_DENIED (5)，而不是 ERROR_SHARING_VIOLATION (32)。
//     Go 的 syscall.Errno.Is 把 ERROR_ACCESS_DENIED 映射成 fs.ErrPermission。
//   - 跨卷 rename 返回 ERROR_NOT_SAME_DEVICE (17)，Go **没有**为它定义
//     syscall.EXDEV（Windows 上的 syscall.EXDEV 是「臆造错误」列表里的占位值
//     0x20000000，永远不等于任何真实 errno）。
//     于是 errors.Is(err, syscall.EXDEV) 在 Windows 上**恒为 false**。
//
// 后果（issue #226 修复的 Windows 侧缺口）：跨卷或文件被占用时，
// IsReplaceableErr 认不出来 → auth.SaveAtomic 不降级、直接报错 →
// **刷新后的 accessToken 不落盘**（磁盘上仍是旧 token）→ 下次启动用旧 token。
var windowsReplaceableErrno = []syscall.Errno{
	winErrNotSameDevice,
	winErrSharingViolation,
	winErrLockViolation,
	syscall.ERROR_ACCESS_DENIED,
}

// CanFallbackToInPlace 报告 rename 失败是否属于「应当降级为原地覆盖」。
//
// Windows 版额外识别 ERROR_NOT_SAME_DEVICE / ERROR_ACCESS_DENIED /
// ERROR_SHARING_VIOLATION / ERROR_LOCK_VIOLATION 与 fs.ErrPermission ——
// 见 windowsReplaceableErrno 的注释。
//
// 安全边界：rename 的失败只可能来自**目标侧**（源 tmp 刚刚写成功），所以
// 「权限类错误 → 降级为原地覆盖」是安全的：目标文件本身可写时能写进去；
// 不可写时 writeInPlace 会返回真实错误，不会静默丢数据。
func CanFallbackToInPlace(err error) bool {
	if err == nil {
		return false
	}
	if IsReplaceableErr(err) { // EXDEV / EBUSY（POSIX 语义，Windows 上一般不命中）
		return true
	}
	for _, errno := range windowsReplaceableErrno {
		if errors.Is(err, errno) {
			return true
		}
	}
	return errors.Is(err, fs.ErrPermission)
}
