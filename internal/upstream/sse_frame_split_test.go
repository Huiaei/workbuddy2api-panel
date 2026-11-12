package upstream

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- 单元层：粘帧拆分 ---------------------------------------------------

// TestSplitConcatenatedJSONTwoObjects 两段 JSON 粘在一行必须切成两段。
func TestSplitConcatenatedJSONTwoObjects(t *testing.T) {
	payload := `{"id":"1","choices":[{"index":0,"delta":{"content":"A"}}]}{"id":"2","choices":[{"index":0,"delta":{"content":"B"}}]}`
	parts := splitConcatenatedJSON(payload)
	if len(parts) != 2 {
		t.Fatalf("应切成 2 段，实际 %d 段: %#v", len(parts), parts)
	}
	for i, p := range parts {
		if !json.Valid([]byte(p)) {
			t.Fatalf("第 %d 段不是合法 JSON: %q", i, p)
		}
	}
	if !strings.Contains(parts[0], `"content":"A"`) || !strings.Contains(parts[1], `"content":"B"`) {
		t.Fatalf("分段内容不对: %#v", parts)
	}
}

// TestSplitConcatenatedJSONBracesInsideString 字符串里的花括号 / 转义引号不能被
// 当成结构边界误切。
func TestSplitConcatenatedJSONBracesInsideString(t *testing.T) {
	payload := `{"a":"he said \"{\"hi\"}\" and \\ escaped","b":[1,2,{"c":3}]}{"d":"x"}`
	parts := splitConcatenatedJSON(payload)
	if len(parts) != 2 {
		t.Fatalf("应切成 2 段，实际 %d 段: %#v", len(parts), parts)
	}
	if !json.Valid([]byte(parts[0])) || !json.Valid([]byte(parts[1])) {
		t.Fatalf("分段不是合法 JSON: %#v", parts)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(parts[0]), &first); err != nil {
		t.Fatalf("首段反序列化失败: %v", err)
	}
	if first["a"] != `he said "{"hi"}" and \ escaped` {
		t.Fatalf("首段字符串内容被误切: %#v", first["a"])
	}
	if !strings.Contains(parts[1], `"d"`) {
		t.Fatalf("次段内容不对: %q", parts[1])
	}
}

// TestSplitConcatenatedJSONSingleObject 只有一个 JSON 时不切。
func TestSplitConcatenatedJSONSingleObject(t *testing.T) {
	payload := `{"id":"1","choices":[{"index":0,"delta":{"content":"A"}}]}`
	parts := splitConcatenatedJSON(payload)
	if len(parts) != 1 {
		t.Fatalf("单个 JSON 不应被切，实际 %d 段: %#v", len(parts), parts)
	}
	if parts[0] != payload {
		t.Fatalf("单段内容应原样返回: %q != %q", parts[0], payload)
	}
}

// TestSplitConcatenatedJSONInvalid 非法 JSON 不切（保持原样，交由上层原样透传）。
func TestSplitConcatenatedJSONInvalid(t *testing.T) {
	for _, payload := range []string{
		`{"a":1} {"b":`,      // 第二段残缺
		`{"a":1} garbage`,    // 第二段不是 JSON
		`{"a":1}{"b":2`,      // 整体残缺
		`not json at all`,    // 完全不是 JSON
		`{"a":1} {"b":2} {`,  // 末尾半截
		`{"a":"unterminated`, // 字符串未闭合
	} {
		parts := splitConcatenatedJSON(payload)
		if len(parts) != 1 {
			t.Errorf("payload %q 不应被切，实际 %d 段: %#v", payload, len(parts), parts)
			continue
		}
		if parts[0] != payload {
			t.Errorf("payload %q 放弃拆分时应原样返回，实际 %q", payload, parts[0])
		}
	}
}

// TestSplitConcatenatedJSONPartCap 段数上限生效：超过 maxSplitParts 段即放弃拆分
// （不把一行膨胀成上百帧）。
func TestSplitConcatenatedJSONPartCap(t *testing.T) {
	// 恰好 maxSplitParts 段 → 可以切。
	exact := make([]string, 0, maxSplitParts)
	for i := 0; i < maxSplitParts; i++ {
		exact = append(exact, fmt.Sprintf(`{"i":%d}`, i))
	}
	parts := splitConcatenatedJSON(strings.Join(exact, ""))
	if len(parts) != maxSplitParts {
		t.Fatalf("恰好 %d 段时应能切分，实际 %d 段", maxSplitParts, len(parts))
	}
	// 超过上限 → 放弃拆分，原样返回。
	over := make([]string, 0, maxSplitParts+1)
	for i := 0; i < maxSplitParts+1; i++ {
		over = append(over, fmt.Sprintf(`{"i":%d}`, i))
	}
	payload := strings.Join(over, "")
	parts = splitConcatenatedJSON(payload)
	if len(parts) != 1 || parts[0] != payload {
		t.Fatalf("超过 %d 段应放弃拆分并原样返回，实际 %d 段", maxSplitParts, len(parts))
	}
}

// TestLooksLikeJSON looksLikeJSON 判定：只认 { / [ 开头（忽略前导空白）。
func TestLooksLikeJSON(t *testing.T) {
	cases := map[string]bool{
		`{"a":1}`:    true,
		`  {"a":1}`:  true,
		"\t[1,2]":    true,
		`"str"`:      false,
		`plain text`: false,
		``:           false,
		`   `:        false,
	}
	for in, want := range cases {
		if got := looksLikeJSON(in); got != want {
			t.Errorf("looksLikeJSON(%q)=%v want %v", in, got, want)
		}
	}
}

// --- 端到端层：粘帧 / 注释帧 ---------------------------------------------

// TestStreamGluedJSONFrameSplit 端到端：上游把两个 JSON chunk 粘进同一行 data，
// 网关必须拆成两个独立帧透传，否则客户端按整行解析必然失败。
func TestStreamGluedJSONFrameSplit(t *testing.T) {
	a := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"AAA"}}]}`
	b := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"BBB"}}]}`
	raw := "data: " + a + " " + b + "\n\n" + "data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	// 每一帧都必须是**独立**的合法 JSON 行（粘帧未拆时整行解析必失败）。
	frames := viewSSE(t, body)
	var contents []string
	for _, f := range frames {
		contents = append(contents, f.content)
	}
	if len(contents) != 2 || contents[0] != "AAA" || contents[1] != "BBB" {
		t.Fatalf("粘帧应拆成 2 帧（AAA/BBB），实际 %#v\nbody=%q", contents, body)
	}
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] 应恰好 1 次: %q", body)
	}
}

// TestStreamGluedJSONFrameSplitThreeSegments 三段粘连同样要拆开。
func TestStreamGluedJSONFrameSplitThreeSegments(t *testing.T) {
	mk := func(c string) string {
		return fmt.Sprintf(`{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":%q}}]}`, c)
	}
	raw := "data: " + mk("A") + " " + mk("B") + " " + mk("C") + "\n\n" + "data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	frames := viewSSE(t, rec.Body.String())
	if len(frames) != 3 {
		t.Fatalf("应拆成 3 帧，实际 %d 帧: %q", len(frames), rec.Body.String())
	}
	for i, want := range []string{"A", "B", "C"} {
		if frames[i].content != want {
			t.Fatalf("第 %d 帧 content=%q want %q", i, frames[i].content, want)
		}
	}
}

// TestStreamInvalidJSONPassthrough 拆不动的非法 JSON 仍按修复前行为原样透传、
// 不计数（不能因为引入拆分而改变降级语义）。
func TestStreamInvalidJSONPassthrough(t *testing.T) {
	valid := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"OK"}}]}`
	raw := "data: " + valid + "\n\n" + "data: {\"broken\n\n" + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data: {"broken`) {
		t.Fatalf("非法 JSON 应原样透传: %q", body)
	}
	// 非法帧不计数，但不应把正常帧挤掉（非法帧本身不参与 JSON 解析断言）。
	var contents []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			// 预期的降级帧（非法 JSON 原样透传）→ 跳过，不计入正常帧。
			continue
		}
		chs, _ := obj["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		ch, _ := chs[0].(map[string]any)
		d, _ := ch["delta"].(map[string]any)
		s, _ := d["content"].(string)
		contents = append(contents, s)
	}
	if len(contents) != 1 || contents[0] != "OK" {
		t.Fatalf("正常帧应保留且只有 1 帧: %#v\nbody=%q", contents, body)
	}
}

// TestStreamOnlyInvalidJSONIsEmptyStream 只有一个非法帧 ⇒ 0 有效帧 ⇒ 走空流
// 错误路径（不因拆分改动而降级语义）。
func TestStreamOnlyInvalidJSONIsEmptyStream(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader("data: {\"broken\n\n"+"data: [DONE]\n\n"))
	if err == nil {
		t.Fatal("0 有效帧应返回空流错误")
	}
	if !IsEmptyStreamError(err) {
		t.Fatalf("应为空流错误，实际 %v", err)
	}
	if !strings.Contains(rec.Body.String(), `data: {"broken`) {
		t.Fatalf("非法 JSON 应原样透传: %q", rec.Body.String())
	}
}
