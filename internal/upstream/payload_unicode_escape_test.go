package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// JS 正则里的 \uXXXX 在 RE2 里不存在；上游按 RE2 校验 pattern，会导致 400/11129
// 整颗 tools 被拒（issue #276）。这里钉住「译成 \x{XXXX}」这一层。
func TestRe2PatternTranslatesJSUnicodeEscapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "REA 路径字段",
			in:   `^[^\uD800-\uDFFF]*$`,
			want: `^[^\x{D800}-\x{DFFF}]*$`,
		},
		{
			name: "带花括号的长码位",
			in:   `\u{1F600}`,
			want: `\x{1F600}`,
		},
		{
			name: "混合普通语法",
			in:   `^(a|\u4E2D)+$`,
			want: `^(a|\x{4E2D})+$`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := re2Pattern(tc.in)
			if !ok {
				t.Fatalf("re2Pattern(%q) 不能编译，想要可编译", tc.in)
			}
			if got != tc.want {
				t.Fatalf("re2Pattern(%q) = %q，想要 %q", tc.in, got, tc.want)
			}
		})
	}
}

// 不含 \u 的 pattern 必须原样返回（正常请求零改动）。
func TestRe2PatternLeavesPlainPatternAlone(t *testing.T) {
	for _, in := range []string{`^[a-z]+$`, `^\d{3}-\d{4}$`, `agent_run_`} {
		got, ok := re2Pattern(in)
		if !ok || got != in {
			t.Fatalf("re2Pattern(%q) = (%q,%v)，想要原样返回", in, got, ok)
		}
	}
}

// 译完仍编不过时返回 false，由调用方丢掉这条 pattern（宁可放宽一条正则，
// 也不能让整包工具被上游否掉）。
func TestRe2PatternRejectsUncompilableResult(t *testing.T) {
	if got, ok := re2Pattern(`\u0041(`); ok {
		t.Fatalf("re2Pattern 对非法正则返回了 (%q,true)，想要 false", got)
	}
}

// 端到端：PrepareBody 发出的 tools[].parameters 里不再残留 \u 转义。
func TestPrepareBodyTranslatesToolPatternUnicode(t *testing.T) {
	body := `{"model":"chat","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string","pattern":"^[^\\uD800-\\uDFFF]*$"}}}}}]}`
	out := PrepareBodyOpt([]byte(body), false)

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("出站 body 不是合法 JSON: %v", err)
	}
	tools, _ := obj["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 长度 = %d，想要 1", len(tools))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	path, _ := props["path"].(map[string]any)
	got, _ := path["pattern"].(string)
	if got != `^[^\x{D800}-\x{DFFF}]*$` {
		t.Fatalf("出站 pattern = %q，想要 ^[^\\x{D800}-\\x{DFFF}]*$", got)
	}
	if strings.Contains(got, `\u`) {
		t.Fatalf("出站 pattern 仍含 \\u 转义: %q", got)
	}
}
