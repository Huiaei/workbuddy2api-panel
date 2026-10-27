package upstream

import (
	"strings"
	"testing"
)

const (
	pathRefP1 = "Tokens prefixed with @ are paths the user explicitly referenced."
	pathRefP2 = "Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."
)

// 两句同时出现（上游共现即拦）⇒ 必须被改写。
func TestPathRefProtocolBothSentencesRewritten(t *testing.T) {
	in := pathRefP1 + " " + pathRefP2
	out := sanitizeText(in)
	if pathRefProtocolPresent(out) {
		t.Fatalf("黑名单文案未被改写: %q", out)
	}
	if out == in {
		t.Fatalf("文本未发生变化")
	}
	// 语义必须保留：@ 路径、workspace root 的意思还在。
	if !strings.Contains(out, "@") {
		t.Errorf("改写丢失了 @ 标记: %q", out)
	}
	if !strings.Contains(out, "workspace") || !strings.Contains(out, "root") {
		t.Errorf("改写丢失了 workspace root 语义: %q", out)
	}
}

// 单句也要改写——上游随时可能收紧成「单句即拦」。
func TestPathRefProtocolSingleSentenceRewritten(t *testing.T) {
	for _, in := range []string{pathRefP1, pathRefP2} {
		out := sanitizeText(in)
		if pathRefProtocolPresent(out) {
			t.Errorf("单句未被改写: %q", out)
		}
	}
}

// 大小写变体同样要拦（上游按大小写不敏感匹配，预检也必须不敏感）。
func TestPathRefProtocolCaseInsensitive(t *testing.T) {
	in := "TOKENS PREFIXED WITH @ ARE PATHS THE USER EXPLICITLY REFERENCED."
	out := sanitizeText(in)
	if pathRefProtocolPresent(out) {
		t.Errorf("大小写变体未被改写: %q", out)
	}
}

// 无关文本零改写。
func TestPathRefProtocolUnrelatedUntouched(t *testing.T) {
	cases := []string{
		"The user mentioned a path in the conversation.",
		"Relative paths are resolved against the current directory.",
		"Tokens prefixed with $ are environment variables.",
		"",
	}
	for _, in := range cases {
		if out := sanitizeText(in); out != in {
			t.Errorf("无关文本被改写: %q -> %q", in, out)
		}
	}
}

// 幂等：二次净化是恒等变换。
func TestPathRefProtocolIdempotent(t *testing.T) {
	in := pathRefP1 + " " + pathRefP2
	once := sanitizeText(in)
	twice := sanitizeText(once)
	if once != twice {
		t.Errorf("非幂等: %q vs %q", once, twice)
	}
}

// DSH 真实提示词里那一整行（第 23 行）必须被改写干净。
func TestPathRefProtocolRealDSHPromptLine(t *testing.T) {
	line := "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. A trailing slash marks a directory: list it when its contents matter. Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. @\"...\" quotes a path containing spaces."
	out := sanitizeText(line)
	if pathRefProtocolPresent(out) {
		t.Fatalf("真实提示词行未被改写: %q", out)
	}
}

// 预检一致性：hasFingerprint 必须对黑名单文案返回 true（否则整段净化被跳过）。
func TestPathRefProtocolPresetConsistency(t *testing.T) {
	if !hasFingerprint(pathRefP1) {
		t.Errorf("hasFingerprint 未命中 P1")
	}
	if !hasFingerprint(pathRefP2) {
		t.Errorf("hasFingerprint 未命中 P2")
	}
	if !hasFingerprint(strings.ToUpper(pathRefP1)) {
		t.Errorf("hasFingerprint 未命中 P1 大写变体")
	}
}
