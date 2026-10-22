package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestConfigExampleIsValid 守卫仓库根目录的 config.example.json 是合法 JSON、
// 且能被 Config 结构解析。
//
// 来由：同族仓库加 features.sanitize_brand_scope 时漏了上一行的逗号，文件
// 变成非法 JSON 而 CI 依然全绿——因为 go build / go test 都不读它，只有真正
// `cp config.example.json config.json` 的人才会撞上，报错现场离提交很远。
// 这个测试把现场挪回 CI。
//
// 顺带守住对外可见性：新增的配置项若只写在 README 里，用户只能从文档反推
// 键名；这里要求它必须在示例里出现。
func TestConfigExampleIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}
	if !json.Valid(raw) {
		t.Fatalf("%s 不是合法 JSON", path)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%s 无法解析为 Config: %v", path, err)
	}
	if !c.Features.SanitizeBlacklistFingerprints {
		t.Error("features.sanitize_blacklist_fingerprints 示例值应为 true")
	}
	// tool_stall_retry（issue #149）缺省 1：示例里必须可见，且不能是 0（0 = 关闭重试）。
	if c.Upstream.ToolStallRetry == 0 {
		t.Error("upstream.tool_stall_retry 示例应为非零（缺省 1），否则用户不知道有这个开关")
	}
}
