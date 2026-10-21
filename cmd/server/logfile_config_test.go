package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLogFileConfigDefaults 日志落盘默认关闭（不落盘），但体积上限有默认值。
func TestLogFileConfigDefaults(t *testing.T) {
	c := Default()
	if c.Logging.LogFile != "" {
		t.Fatalf("默认不应落盘，实际: %q", c.Logging.LogFile)
	}
	if c.Logging.LogFileMaxMB <= 0 {
		t.Fatalf("log_file_max_mb 应有默认，实际: %d", c.Logging.LogFileMaxMB)
	}
	if c.Logging.LogFileBackups <= 0 {
		t.Fatalf("log_file_backups 应有默认，实际: %d", c.Logging.LogFileBackups)
	}
}

// TestLogFileConfigNormalize 非法值回落默认。
func TestLogFileConfigNormalize(t *testing.T) {
	c := Default()
	c.Logging.LogFileMaxMB = -5
	c.Logging.LogFileBackups = 0
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Logging.LogFileMaxMB <= 0 || c.Logging.LogFileBackups <= 0 {
		t.Fatalf("非法值应回落默认: max=%d backups=%d",
			c.Logging.LogFileMaxMB, c.Logging.LogFileBackups)
	}
}

// TestLogFileConfigSurvivesJSONRoundTrip 落盘路径能从 config.json 读回来。
func TestLogFileConfigSurvivesJSONRoundTrip(t *testing.T) {
	raw := `{"listen":":7863","logging":{"log_file":"./data/gateway.log","log_file_max_mb":16,"log_file_backups":5}}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Logging.LogFile != "./data/gateway.log" {
		t.Fatalf("log_file 读取失败: %q", c.Logging.LogFile)
	}
	if c.Logging.LogFileMaxMB != 16 || c.Logging.LogFileBackups != 5 {
		t.Fatalf("落盘参数读取失败: %d / %d",
			c.Logging.LogFileMaxMB, c.Logging.LogFileBackups)
	}
}

// TestConfigExampleHasLogFileKeys config.example.json 里有这两个键（可发现性）。
func TestConfigExampleHasLogFileKeys(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("config.example.json 不是合法 JSON: %v", err)
	}
	lg, ok := m["logging"].(map[string]any)
	if !ok {
		t.Fatal("config.example.json 缺 logging 段")
	}
	if _, ok := lg["log_file"]; !ok {
		t.Fatal("config.example.json 的 logging 段缺 log_file")
	}
	if _, ok := lg["log_file_max_mb"]; !ok {
		t.Fatal("config.example.json 的 logging 段缺 log_file_max_mb")
	}
}
