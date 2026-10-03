package main

import (
	"slices"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 老配置（无 cn 段）必须保留内置 CN base，零回归。
func TestCNChatBaseDefaultKeepsBuiltin(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	up := upstream.New()
	want := up.ChatBaseCN
	applyCNChatBase(up, cfg)
	if up.ChatBaseCN != want {
		t.Fatalf("ChatBaseCN=%q want builtin %q", up.ChatBaseCN, want)
	}
}

func TestCNChatBaseOverride(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"cn":{"chat_base":"https://www.workbuddy.cn"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	up := upstream.New()
	applyCNChatBase(up, cfg)
	if up.ChatBaseCN != "https://www.workbuddy.cn" {
		t.Fatalf("ChatBaseCN=%q want override", up.ChatBaseCN)
	}
	// global base 不受 cn 覆盖影响。
	if up.ChatBaseGlobal != "" {
		t.Fatalf("ChatBaseGlobal=%q want untouched", up.ChatBaseGlobal)
	}
}

// cn.chat_base 只在装配期生效，面板保存后若被改动须提示重启。
//
// 语义随 #104 改为「按与运行中配置的实际差异列出」：cn.chat_base 没变就不必
// 提示重启，变了就必须提示。
func TestCNChatBaseRequiresRestart(t *testing.T) {
	current, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("parse current: %v", err)
	}
	next, err := ParseConfig([]byte(`{"cn":{"chat_base":"https://www.workbuddy.cn"}}`))
	if err != nil {
		t.Fatalf("parse next: %v", err)
	}
	if !slices.Contains(restartRequiredFields(current, next), "cn.chat_base") {
		t.Fatal("cn.chat_base missing from restart-required fields")
	}
	// 没改动时不应提示（装配期字段的提示只在真的变了时出现）。
	if slices.Contains(restartRequiredFields(current, current), "cn.chat_base") {
		t.Fatal("cn.chat_base reported although unchanged")
	}
}
