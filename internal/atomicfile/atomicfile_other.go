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
