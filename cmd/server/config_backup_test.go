package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 本文件钉住**交叉审计第二轮 Q4.5** 抓到的启动级数据风险：
//
// 降级路径（rename 不可用时的原地覆盖）是 **O_TRUNC 覆写**，**非原子**：
// 进程在写一半时被杀 ⇒ 磁盘上留下**半截** config.json。而 config.json 半截
// ⇒ Load 解析失败 ⇒ main.go log.Fatalf("load config: …") ⇒ **服务起不来**：
//
//	[half-config] 完整配置加载 ok listen=:7863
//	[half-config] 半截配置加载 err=parse config: unexpected end of JSON input
//	[half-config] 空文件   加载 err=parse config: unexpected end of JSON input
//
// 修法（三层）：
//  1) 落盘前先留一份 **.bak** 备份（上一份**完整**配置）；
//  2) 启动时解析失败 → 自动回退到 .bak，并打 WARN（服务照常起来）；
//  3) .bak 也没有 → 明确提示「配置已损坏」并指出恢复路径（而不是裸 JSON 错误）。
//
// 注意方向：**绝不**用「半截配置」启动（那会让面板/调度器拿着缺字段的配置跑，
// 比启动失败更难排查）；宁可回退到上一份完整配置。

// TestLoadFallsBackToBackupWhenConfigTruncated 主配置半截 ⇒ 自动回退 .bak。
func TestLoadFallsBackToBackupWhenConfigTruncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// 先落一份完整配置（走 WriteDefault 的同款序列化路径）。
	good := Default()
	good.Listen = ":7899"
	raw := mustMarshalConfig(t, good)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// 备份 = 上一份完整配置。
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// 模拟「原地覆盖写到一半进程被杀」：主文件变成半截。
	if err := os.WriteFile(path, []byte(`{"listen":`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("半截配置应回退到备份并正常启动，实际报错: %v", err)
	}
	if cfg.Listen != ":7899" {
		t.Fatalf("回退后应使用备份里的配置，listen=%q（want :7899）", cfg.Listen)
	}
}

// TestLoadWithTruncatedConfigAndNoBackup 备份也没有 ⇒ 明确报错而不是静默起。
func TestLoadWithTruncatedConfigAndNoBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("半截配置且无备份时必须报错（不得静默按默认值启动）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "config.json") {
		t.Fatalf("错误信息应指出是哪个配置文件坏了: %q", msg)
	}
	// 错误信息必须可操作：告诉用户怎么恢复。
	if !strings.Contains(msg, ".bak") && !strings.Contains(msg, "修复") && !strings.Contains(msg, "恢复") {
		t.Fatalf("错误信息必须给出恢复指引: %q", msg)
	}
}

// TestLoadWithEmptyConfigFile 空文件同样按「损坏」处理（.bak 可救）。
func TestLoadWithEmptyConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	good := Default()
	good.Listen = ":7900"
	raw := mustMarshalConfig(t, good)
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("空配置应回退到备份: %v", err)
	}
	if cfg.Listen != ":7900" {
		t.Fatalf("回退后 listen=%q（want :7900）", cfg.Listen)
	}
}

// TestWriteDefaultKeepsPreviousConfigAsBackup WriteDefault 之外，落盘路径
// （saveConfig）也必须在覆盖前留备份。
func TestSaveConfigKeepsBackupBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	old := Default()
	old.Listen = ":7001"
	old.APIKey = "sk-old"
	rawOld := mustMarshalConfig(t, old)
	if err := os.WriteFile(path, rawOld, 0o600); err != nil {
		t.Fatal(err)
	}

	// 面板提交新配置（改 listen）。
	submitted := []byte(`{"listen":":7002"}`)
	live := livecfg.New(livecfg.Snapshot{})
	p := pool.New("")
	defer p.Close()
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	if _, err := saveConfig(submitted, path, live, p, up, sch, nil, dir); err != nil {
		t.Fatalf("saveConfig 失败: %v", err)
	}

	// 备份应存在且内容是**旧**的完整配置。
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("保存后应留下 .bak 备份: %v", err)
	}
	if !strings.Contains(string(backup), ":7001") {
		t.Fatalf("备份应是覆盖前的旧配置，实际: %s", string(backup)[:min(200, len(backup))])
	}
	// 新配置已生效。
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cur), ":7002") {
		t.Fatalf("新配置未落盘: %s", string(cur)[:min(200, len(cur))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// mustMarshalConfig 把配置序列化成 JSON（测试用）。
func mustMarshalConfig(t *testing.T, c *Config) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatalf("序列化配置失败: %v", err)
	}
	return raw
}
