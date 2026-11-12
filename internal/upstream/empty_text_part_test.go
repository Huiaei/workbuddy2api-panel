package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// decodeMessages 把 body 解成 messages 切片，供补位断言使用。
func decodeMessages(t *testing.T, body []byte) []any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("prepareBody 结果不是合法 JSON: %v\n%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("prepareBody 结果缺 messages：%s", body)
	}
	return msgs
}

// TestGuardEmptyTextPartsMatrix 形态矩阵：把上游实测的六种形态逐条钉死。
//
// 撤掉 payload.go 里的 guardEmptyTextParts 接线 ⇒ 前三条（空串/null/缺失）失败。
func TestGuardEmptyTextPartsMatrix(t *testing.T) {
	cases := []struct {
		name    string
		content any
		want    string // 期望第一段 text 的值；空串表示「保持为空/不应变成补位值」
		changed bool
	}{
		{"空串补位", []any{map[string]any{"type": "text", "text": ""}}, " ", true},
		{"null 补位", []any{map[string]any{"type": "text", "text": nil}}, " ", true},
		{"缺失 text 补位", []any{map[string]any{"type": "text"}}, " ", true},
		{"纯空白不动", []any{map[string]any{"type": "text", "text": "   "}}, "   ", false},
		{"未识别 type 不动", []any{map[string]any{"type": "textarea", "text": ""}}, "", false},
		{"非字符串 text 不动", []any{map[string]any{"type": "text", "text": 0.0}}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msgs := []any{map[string]any{"role": "user", "content": c.content}}
			guardEmptyTextParts(msgs)
			parts := msgs[0].(map[string]any)["content"].([]any)
			got := parts[0].(map[string]any)["text"]
			if c.changed {
				if got != c.want {
					t.Fatalf("text=%v want %q", got, c.want)
				}
				return
			}
			// 「不动」类：值必须还是原值（纯空白保持 "   "，其余保持 nil/缺失/0）
			if s, isStr := got.(string); isStr && s == " " && got != c.want {
				t.Fatalf("不该补位的形态被补位了：text=%q", s)
			}
		})
	}
}

// TestGuardEmptyTextPartsObjectContentUntouched 非数组 content（单个对象）上游不嗅探，
// 我们也不动它。
func TestGuardEmptyTextPartsObjectContentUntouched(t *testing.T) {
	obj := map[string]any{"type": "text", "text": ""}
	msgs := []any{map[string]any{"role": "user", "content": obj}}
	guardEmptyTextParts(msgs)
	got := msgs[0].(map[string]any)["content"].(map[string]any)
	if got["text"] != "" {
		t.Fatalf("对象形态 content 不该被补位，text=%v", got["text"])
	}
}

// TestGuardEmptyTextPartsSniffsStringEnvelope 字符串 content 以 `[` 开头且能解析成
// 非空对象数组时，上游会把它当 parts 解析 ⇒ 我们按同口径嗅探并补位后重新序列化。
//
// 这是「工具结果里抓到的表单/按钮数组」那种真实形态。
func TestGuardEmptyTextPartsSniffsStringEnvelope(t *testing.T) {
	raw := `[{"type":"text","text":""},{"type":"text","text":"按钮 A"}]`
	msgs := []any{map[string]any{"role": "user", "content": raw}}
	guardEmptyTextParts(msgs)
	out, ok := msgs[0].(map[string]any)["content"].(string)
	if !ok {
		t.Fatalf("嗅探命中后 content 应该被重新序列化成字符串")
	}
	var parts []any
	if err := json.Unmarshal([]byte(out), &parts); err != nil {
		t.Fatalf("重新序列化结果不是合法 JSON: %v (%s)", err, out)
	}
	if got := parts[0].(map[string]any)["text"]; got != " " {
		t.Fatalf("嗅探形态未被补位：text=%v", got)
	}
	// 第二段是正常内容，必须原样保留
	if got := parts[1].(map[string]any)["text"]; got != "按钮 A" {
		t.Fatalf("正常段被改动：text=%v", got)
	}
}

// TestGuardEmptyTextPartsSniffRejects 嗅探反例：这些字符串上游不会当 parts 解析，
// 我们一个字都不能动（改了就篡改用户数据）。
func TestGuardEmptyTextPartsSniffRejects(t *testing.T) {
	cases := map[string]string{
		"普通文本":      "hello world",
		"空数组":       "[]",
		"解析失败":      "[not json",
		"元素不是对象":    `["a","b"]`,
		"前面有非空白字符":  "x[{\"type\":\"text\",\"text\":\"\"}]",
		"顶层是对象不是数组": `{"type":"text","text":""}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			msgs := []any{map[string]any{"role": "user", "content": content}}
			guardEmptyTextParts(msgs)
			if got := msgs[0].(map[string]any)["content"]; got != content {
				t.Fatalf("不该被改动的字符串被改了：%v", got)
			}
		})
	}
}

// TestGuardEmptyTextPartsKeepsOtherParts 图片等非 text 段必须按引用原样保留：
// 补位只克隆要改的 text 段，不重建整条消息。
func TestGuardEmptyTextPartsKeepsOtherParts(t *testing.T) {
	img := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/y.png"}}
	msgs := []any{map[string]any{"role": "user", "content": []any{
		img, map[string]any{"type": "text", "text": ""},
	}}}
	guardEmptyTextParts(msgs)
	parts := msgs[0].(map[string]any)["content"].([]any)
	if got, ok := parts[0].(map[string]any); !ok || got["type"] != "image_url" || got["image_url"] == nil {
		t.Fatalf("image 段不该被克隆/替换：%v", parts[0])
	}
	// 按引用保留：map 不可比较，用「改动原 map 能反映到输出」来证明未被克隆。
	img["probe"] = "x"
	if got := parts[0].(map[string]any)["probe"]; got != "x" {
		t.Fatalf("image 段被克隆了（改动原 map 未反映到 out）")
	}
	if got := parts[1].(map[string]any)["text"]; got != " " {
		t.Fatalf("text 段未被补位：%v", got)
	}
}

// TestPrepareBodyGuardsEmptyTextPart 端到端接线：PrepareBody* 真正把补位跑在出站 body 上。
//
// 撤掉 payload.go 的接线 ⇒ 本用例失败（出站 body 里仍是空串）。
func TestPrepareBodyGuardsEmptyTextPart(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`)
	out := PrepareBodyOpt(src, false)
	msgs := decodeMessages(t, out)
	parts := msgs[0].(map[string]any)["content"].([]any)
	if got := parts[0].(map[string]any)["text"]; got != emptyTextPartFill {
		t.Fatalf("出站 body 里空 text 段未补位：%v", got)
	}
	if !strings.Contains(string(out), `"text":" "`) {
		t.Fatalf("出站 body 未见补位值：%s", out)
	}
}

// TestPrepareBodyLeavesNormalRequestAlone 普通请求零影响：无空段时 messages 一字不动。
func TestPrepareBodyLeavesNormalRequestAlone(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}]}`)
	out := PrepareBodyOpt(src, false)
	msgs := decodeMessages(t, out)
	if got := msgs[0].(map[string]any)["content"]; got != "你好" {
		t.Fatalf("普通文本被改动：%v", got)
	}
}

// TestClassifyEmptyContent11151 11151 归 ErrEmptyContent（请求级），且不能把
// 「带 11151 字样的非 400」「500 + 11151」「400 + 别的业务码」误分类。
func TestClassifyEmptyContent11151(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"400 + 11151 字符串码", 400, `{"code":11151,"msg":"a message has empty content"}`, ErrEmptyContent},
		{"400 + 11151 数字串码", 400, `{"code":"11151","msg":"a message has empty content"}`, ErrEmptyContent},
		{"500 + 11151 仍是上游故障", 500, `{"code":11151,"msg":"a message has empty content"}`, ErrServer},
		{"400 + 别的业务码", 400, `{"code":11200,"msg":"other"}`, ErrClient},
		{"200 不受影响", 200, `{"code":0,"msg":""}`, ErrNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Fatalf("Classify(%d,%s)=%v want %v", c.status, c.body, got, c.want)
			}
		})
	}
}

// TestEmptyTextPartFillIsNonEmpty 补位值必须非空：上游只判「长度是否为零」，
// 填空串等于没补（变异验证：把常量改回 "" ⇒ 本用例与矩阵/嗅探用例一起变红）。
func TestEmptyTextPartFillIsNonEmpty(t *testing.T) {
	if emptyTextPartFill == "" {
		t.Fatalf("补位值不能是空串——上游只判零长度，空串等于没补")
	}
	if strings.TrimSpace(emptyTextPartFill) != "" {
		t.Fatalf("补位值不该带可见字符（会进模型上下文）：%q", emptyTextPartFill)
	}
}

// TestErrEmptyContentString 错误码字符串稳定（面板/日志/客户端会读它）。
func TestErrEmptyContentString(t *testing.T) {
	if got := ErrEmptyContent.String(); got != "empty_content" {
		t.Fatalf("ErrEmptyContent.String()=%q want empty_content", got)
	}
}

// TestGatewayHintEmptyContent hint 必须指引「删掉那条消息/换新会话」，而不是换号重试。
func TestGatewayHintEmptyContent(t *testing.T) {
	h := GatewayHint(ErrEmptyContent, "a message has empty content", HintContext{})
	if !strings.Contains(h, "empty text block") {
		t.Fatalf("hint 未说明空 text 段：%q", h)
	}
	if !strings.Contains(h, "start a new conversation") {
		t.Fatalf("hint 未给出可操作出路：%q", h)
	}
	if strings.Contains(h, "retry") && !strings.Contains(h, "instead of retrying") {
		t.Fatalf("hint 不该鼓励直接重试：%q", h)
	}
}

// 确保 http 包被使用（形态矩阵里 status 常量来自它）。
var _ = http.StatusBadRequest
