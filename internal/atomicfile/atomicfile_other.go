//go:build !windows

package atomicfile

// CanFallbackToInPlace 报告 rename 失败是否属于「应当降级为原地覆盖」。
//
// 非 Windows 平台：只有 EXDEV（跨设备）与 EBUSY（单文件 bind mount 挂载点）。
func CanFallbackToInPlace(err error) bool {
	if err == nil {
		return false
	}
	return IsReplaceableErr(err)
}

// isTargetBusyErr 报告 rename 失败是否属于「**目标侧临时占用**」（值得重试）。
//
// 非 Windows 平台：POSIX 的 rename(2) 在目标被其它进程打开时**不会**失败
// （它只要求目录可写），失败只来自 EXDEV（跨设备）与 EBUSY（单文件挂载点）
// —— 这两个都是**结构性问题**，重试没有意义。
//
// 因此非 Windows 上恒返回 false：retryRename 不会空转，行为与从前完全一致。
func isTargetBusyErr(err error) bool { return false }
