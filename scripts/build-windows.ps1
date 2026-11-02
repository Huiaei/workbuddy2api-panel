param(
    # Console = 只构建控制台版（带命令行窗口）；Tray = 只构建托盘版（无窗口）；
    # All = 两者都构建。
    [ValidateSet("Console", "Tray", "All")]
    [string]$Mode = "All"
)

$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $env:CGO_ENABLED = "0"
    if ($Mode -in @("Console", "All")) {
        go build -trimpath -ldflags "-s -w" -o wb2api.exe ./cmd/server
        if ($LASTEXITCODE -ne 0) { throw "console build failed" }
    }
    if ($Mode -in @("Tray", "All")) {
        # -H=windowsgui：GUI 子系统，启动时没有命令行窗口。
        # -tags tray：启用系统托盘（图标 + 右键菜单）。
        go build -tags tray -trimpath -ldflags "-s -w -H=windowsgui" -o wb2api-tray.exe ./cmd/server
        if ($LASTEXITCODE -ne 0) { throw "tray build failed" }
    }
    Write-Host "构建完成（模式：$Mode）"
} finally {
    Pop-Location
}
