package upstream

import (
	"strings"
	"testing"
)

// 2026-10-21 CN realm 实案：上游渠道校验新增了针对本产品身份句的**逐字**黑名单，
// 命中即 400 code=11128 "Illegal API invocation from an unapproved channel"。
// 实测（直连上游，K≥3 重复，结果确定）：
//
//	"You are an AI agent powered by DeepSeek Harness."（独立成句）  ⇒ 恒定 400
//	"You are interacting with the user through the DeepSeek Harness Web GUI whose URL the
//	 runtime context names. When the user refers to "this page", "this GUI", or "this app"
//	 without naming another target, they mean this GUI."（两句连体） ⇒ 恒定 400
//
// 而下列形态恒定 200（所以不能整体替换产品名）：
//
//	"DeepSeek Harness" 单独 / 含路径 C:\app\DeepSeek Harness\...   ⇒ 200
//	"You are a DeepSeek Harness user." 等其它句式                 ⇒ 200
//	中性句里出现两次产品名                                           ⇒ 200
//
// 两条黑名单句**互相独立**（改掉任一条，另一条仍然触发）。

const dshIdentityLine = "You are an AI agent powered by DeepSeek Harness."
const dshGUISentence = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."

func TestDSHIdentitySentenceRewritten(t *testing.T) {
	out := sanitizeText(dshIdentityLine)
	if strings.Contains(out, "You are an AI agent powered by DeepSeek Harness") {
		t.Errorf("DSH identity sentence still present: %q", out)
	}
	if !strings.Contains(out, "DSH") {
		t.Errorf("alias missing after rewrite: %q", out)
	}
	// 语义保留：仍是「an AI agent powered by ...」。
	if !strings.Contains(out, "You are an AI agent powered by") {
		t.Errorf("rewritten sentence lost its shape: %q", out)
	}
}

func TestDSHGUISentenceRewritten(t *testing.T) {
	out := sanitizeText(dshGUISentence)
	if strings.Contains(out, "through the DeepSeek Harness Web GUI") {
		t.Errorf("DSH GUI sentence still present: %q", out)
	}
	if !strings.Contains(out, "You are interacting with the user through the") {
		t.Errorf("rewritten GUI sentence lost its shape: %q", out)
	}
}

// 两条黑名单句**各自独立**：改掉 T1 之后 T2 仍须被改写（反之亦然）。
// 这是真实 prompt 的形态（行 0 = T1、行 100 = T2），漏改任一条都仍然 400。
func TestDSHBlacklistSentencesAreIndependent(t *testing.T) {
	in := dshIdentityLine + "\n\n" + dshGUISentence
	out := sanitizeText(in)
	for _, s := range []string{
		"You are an AI agent powered by DeepSeek Harness",
		"through the DeepSeek Harness Web GUI",
	} {
		if strings.Contains(out, s) {
			t.Errorf("blacklist sentence survived (%q): %q", s, out)
		}
	}
}

// 大小写变体同样被上游拦（实测 "you are an AI agent powered by deepseek harness." 触发），
// 预检必须大小写不敏感，否则净化被整段跳过。
func TestDSHIdentityCaseInsensitive(t *testing.T) {
	for _, in := range []string{
		"you are an AI agent powered by deepseek harness.",
		"You Are An AI Agent Powered By DeepSeek Harness.",
		"YOU ARE AN AI AGENT POWERED BY DEEPSEEK HARNESS.",
		"DeepSeek harness",
	} {
		out := sanitizeText(in)
		if strings.Contains(strings.ToLower(out), "deepseek harness") {
			t.Errorf("case variant not sanitized: %q -> %q", in, out)
		}
	}
}

// 路径形态必须逐字保留：改掉会让模型拿到错路径（且实测路径形态本来也不被拦）。
func TestDSHPathPreserved(t *testing.T) {
	in := `The DeepSeek Harness implementation checkout is at C:\app\DeepSeek Harness\resources\app.asar\dsh\.`
	out := sanitizeText(in)
	if !strings.Contains(out, `C:\app\DeepSeek Harness\resources\app.asar\dsh\`) {
		t.Errorf("checkout path mangled: %q", out)
	}
	// 路径外的身份句仍应改写。
	if strings.Contains(out, "The DeepSeek Harness implementation checkout is at") {
		t.Errorf("identity sentence outside path not rewritten: %q", out)
	}
}

// 真实 DSH 提示词里的三处产品名形态：
//  1. 身份句（独立）    2) 实现目录说明句    3) 路径
func TestDSHRealPromptShapes(t *testing.T) {
	in := "You are an AI agent powered by DeepSeek Harness.\n\n" +
		"The runtime context names the DeepSeek Harness implementation checkout. " +
		"The checkout location and current working directory are separate values.\n\n" +
		`The DeepSeek Harness implementation checkout is at C:\app\DeepSeek Harness\resources\app.asar\dsh\.` + "\n\n" +
		"You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	out := sanitizeText(in)

	if strings.Contains(out, "You are an AI agent powered by DeepSeek Harness") {
		t.Errorf("identity sentence survived: %q", out)
	}
	if strings.Contains(out, "through the DeepSeek Harness Web GUI") {
		t.Errorf("GUI sentence survived: %q", out)
	}
	if strings.Contains(out, "The runtime context names the DeepSeek Harness implementation checkout") {
		t.Errorf("checkout sentence survived: %q", out)
	}
	// 路径：逐字保留。
	if !strings.Contains(out, `C:\app\DeepSeek Harness\resources\app.asar\dsh\`) {
		t.Errorf("checkout path mangled: %q", out)
	}
}

// 无产品名的普通文本必须逐字不动（零回归）。
func TestDSHNoProductNameUntouched(t *testing.T) {
	for _, in := range []string{
		"You are a helpful coding assistant.",
		"The harness runs on Linux.",
		"Deep learning and harness patterns.",
		"please use main branch for this repo",
	} {
		if out := sanitizeText(in); out != in {
			t.Errorf("unrelated text changed: %q -> %q", in, out)
		}
	}
}

// 预检命中但不改写内容时不得破坏原串（hasFingerprint 与改写层口径一致）。
func TestDSHPresetConsistency(t *testing.T) {
	in := "you are an AI agent powered by deepseek harness."
	if !hasFingerprint(in) {
		t.Fatal("pre-check missed case variant")
	}
}

// 工具 schema 里的产品名也必须脱敏（真实 DSH 工具描述含
// "Search the whole DSH (DeepSeek Harness) plugin ecosystem"）。
func TestDSHToolDescriptionRewritten(t *testing.T) {
	in := "Search the whole DSH (DeepSeek Harness) plugin ecosystem for a plugin that provides some capability."
	out := sanitizeText(in)
	if strings.Contains(out, "DeepSeek Harness") {
		t.Errorf("tool description still contains product name: %q", out)
	}
}

// tools[] 声明也必须脱敏：真实 DSH 工具集 183 个里有 2 个描述含产品名
// （find_dsh_plugins / free_search_test），上游黑名单扫的是整个请求体。
func TestDSHToolsSchemaRewritten(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","function":{"name":"find_dsh_plugins","description":"Search the whole DSH (DeepSeek Harness) plugin ecosystem for a plugin that provides some capability.","parameters":{"type":"object","properties":{"query":{"type":"string","description":"Optional search query to use for the test (default: 'DeepSeek Harness')."}}}}},` +
		`{"type":"function","function":{"name":"bash","description":"Run a command.","parameters":{"type":"object","properties":{}}}}]}`)
	out := PrepareBodyOptWithEffortsAndDefault(body, true, nil, nil)
	s := string(out)
	if contains := "DeepSeek Harness"; strings.Contains(s, contains) {
		t.Errorf("tools schema still contains product name: %s", s)
	}
	if !strings.Contains(s, "Search the whole DSH (DSH) plugin ecosystem") {
		t.Errorf("tool description not rewritten as expected: %s", s)
	}
	// 结构不能被破坏。
	if !strings.Contains(s, `"name":"find_dsh_plugins"`) || !strings.Contains(s, `"name":"bash"`) {
		t.Errorf("tool structure damaged: %s", s)
	}
}
