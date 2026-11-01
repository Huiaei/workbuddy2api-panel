// headorder_test.go 头顺序重排的回归测试。
//
// 判据不用手写期望值，而是**直接拿官方真机夹具当 oracle**：把夹具的头顺序
// 打乱再喂进重排函数，输出必须逐行等于夹具原文。夹具本身来自真机抓包，所以
// 这条链路红 == 与官方顺序不符。
//
// 夹具原文内嵌在本文件里（写范围不含 testdata/）；若 testdata/ 下存在同名
// 文件，会额外断言二者一致，防止内嵌副本漂移。
package upstream

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 官方真机头夹具原文（逐字节抄自 reporter fork 的 testdata/official_*_head.txt）。
const (
	fixtureChatHead = `POST /v2/chat/completions HTTP/1.1
Accept: application/json
Content-Type: application/json
X-Requested-With: XMLHttpRequest
x-stainless-arch: x64
x-stainless-lang: js
x-stainless-os: Windows
x-stainless-package-version: 6.25.0
x-stainless-retry-count: 0
x-stainless-runtime: node
x-stainless-runtime-version: v22.21.1
X-Conversation-ID: f7c6c392-e016-43bb-beca-ab918b436668
X-Agent-Purpose: conversation
X-IDE-Type: WorkBuddy
X-IDE-Name: WorkBuddy
X-IDE-Version: 5.9.3
X-Agent-Intent: craft
X-Agent-Runtime: local-agent
X-Private-Data: false
acp-connection-id: 01a194ad-6b5d-7394-92ad-61e04eb3a3df
x-codebuddy-request: 1
X-Request-ID: 01a194adc6bc7205b0d188e0c9b6a728
X-Conversation-Message-ID: 01a194adc6bc7205b0d188e0c9b6a728
X-Conversation-Request-ID: 01a194ad31c27d81804ee9915cf954fb
X-Root-Request-ID: 01a194ad31c27d81804ee9915cf954fb
X-Agent-Type: main
baggage: buddyId=,buddyName=,codebuddy.session_id=f7c6c392-e016-43bb-beca-ab918b436668,codebuddy.conversation_request_id=01a194ad31c27d81804ee9915cf954fb
Content-Encoding: gzip
traceparent: 00-7458e488eb564b059622db42023ae293-82e31fea372dea0e-01
b3: 7458e488eb564b059622db42023ae293-82e31fea372dea0e-1-57587f4a3ce47317
X-B3-TraceId: 7458e488eb564b059622db42023ae293
X-B3-ParentSpanId: 57587f4a3ce47317
X-B3-SpanId: 82e31fea372dea0e
X-B3-Sampled: 1
X-Trace-ID: 7458e488eb564b059622db42023ae293
X-Product: SaaS
User-Agent: WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0
Authorization: Bearer <REDACTED>
X-User-Id: 11f49997-8875-4ce2-ae68-d093e3088524
X-Domain: www.workbuddy.cn
Content-Length: 54062
Accept-Encoding: gzip, compress, deflate, br
Host: www.workbuddy.cn
Connection: keep-alive`

	fixtureReportHead = `POST /v2/report HTTP/1.1
Accept: application/json, text/plain, */*
Content-Type: application/json;charset=UTF-8
X-Request-ID: 4891f0b179cc4959be94b2c867c16e3c
X-Product: SaaS
User-Agent: WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0
Authorization: Bearer <REDACTED>
X-User-Id: 11f49997-8875-4ce2-ae68-d093e3088524
X-Domain: www.workbuddy.cn
Content-Length: 765
Host: www.workbuddy.cn
Connection: keep-alive`

	fixtureBusyHead = `POST /v2/chat/busy HTTP/1.1
Accept: application/json, text/plain, */*
Content-Type: application/json
User-Agent: WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0
X-Request-ID: d7c563ab9dc0414fb0c99a4de06a0a85
X-Product: SaaS
Authorization: Bearer <REDACTED>
X-User-Id: 11f49997-8875-4ce2-ae68-d093e3088524
X-Domain: www.workbuddy.cn
Content-Length: 25
Host: www.workbuddy.cn
Connection: keep-alive`

	fixtureV3ConfigHead = `GET /v3/config HTTP/1.1
Accept: application/json, text/plain, */*
Connection: close
User-Agent: WorkBuddy/5.9.3 CLI/2.165.0
X-Request-ID: c16e715dd3a042cb8f58616fd5b6fe71
X-Product: SaaS
Host: www.workbuddy.cn`
)

// splitFixture 把夹具切成 (请求行, []头行)。
func splitFixture(t *testing.T, name string) (string, []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读夹具 %s: %v", name, err)
	}
	return splitHeadText(t, string(raw))
}

// splitHeadText 把夹具文本切成 (请求行, []头行)。
func splitHeadText(t *testing.T, raw string) (string, []string) {
	t.Helper()
	var first string
	var rest []string
	for i, ln := range strings.Split(raw, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			continue
		}
		if i == 0 {
			first = ln
			continue
		}
		rest = append(rest, ln)
	}
	return first, rest
}

// shuffleHeaders 确定性反转（不用随机：测试必须可复现）。
func shuffleHeaders(h []string) []string {
	out := make([]string, len(h))
	for i := range h {
		out[i] = h[len(h)-1-i]
	}
	return out
}

// buildBlock 由请求行 + 头行构造一个完整头块（含结尾空行）。
func buildBlock(first string, headers []string) []byte {
	var b bytes.Buffer
	b.WriteString(first + "\r\n")
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// TestEmbeddedFixturesMatchTestdata 若 testdata 下存在官方夹具，断言与内嵌副本
// 一致（防止内嵌副本悄悄漂移）。
func TestEmbeddedFixturesMatchTestdata(t *testing.T) {
	pairs := []struct{ file, embedded string }{
		{"official_chat_head.txt", fixtureChatHead},
		{"official_report_cn_head.txt", fixtureReportHead},
		{"official_chat_busy_head.txt", fixtureBusyHead},
		{"official_v3config_head.txt", fixtureV3ConfigHead},
	}
	for _, p := range pairs {
		t.Run(p.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", p.file))
			if err != nil {
				t.Skipf("testdata/%s 不存在（写范围不含 testdata/）：%v", p.file, err)
			}
			got := strings.TrimRight(string(raw), "\r\n")
			if got != p.embedded {
				t.Errorf("内嵌夹具与 testdata/%s 不一致", p.file)
			}
		})
	}
}

// TestReorderHeadBlockMatchesOfficial 每张夹具：打乱 → 重排 → 必须等于原文。
func TestReorderHeadBlockMatchesOfficial(t *testing.T) {
	raws := map[string]string{
		"chat":     fixtureChatHead,
		"report":   fixtureReportHead,
		"busy":     fixtureBusyHead,
		"v3config": fixtureV3ConfigHead,
	}
	for name, raw := range raws {
		first, headers := split(t, raw)
		t.Run(name, func(t *testing.T) {
			got := reorderHeadBlock(buildBlock(first, shuffleHeaders(headers)))
			wantBlock := buildBlock(first, headers)
			if string(got) != string(wantBlock) {
				t.Errorf("重排结果与官方不一致\n--- got ---\n%s\n--- want ---\n%s", got, wantBlock)
			}
		})
	}
}

// split 拆夹具文本为 (请求行, 头行)。
func split(t *testing.T, raw string) (string, []string) {
	t.Helper()
	return splitHeadText(t, raw)
}

// mustFixture 拆夹具文本（保持旧名别名，便于 e2e 文件引用）。
func mustFixture(t *testing.T, raw string) (string, []string) {
	t.Helper()
	return splitHeadText(t, raw)
}

// TestReorderHeadBlockUnknownPathUntouched 顺序表外的路径必须原样返回。
func TestReorderHeadBlockUnknownPathUntouched(t *testing.T) {
	in := buildBlock("GET /v9/nothing HTTP/1.1", []string{"Zeta: 1", "Alpha: 2"})
	if got := reorderHeadBlock(in); string(got) != string(in) {
		t.Errorf("未知路径不该重排:\n%s", got)
	}
}

// TestReorderHeadBlockExtraHeadersAppended 表外头保持原序追加在末尾。
func TestReorderHeadBlockExtraHeadersAppended(t *testing.T) {
	first, headers := mustFixture(t, fixtureV3ConfigHead)
	// 表外头放在最前；反转输入后 X-Another 在 X-Brand-New 之前
	withExtra := append([]string{"X-Brand-New: 1", "X-Another: 2"}, headers...)
	shuffled := shuffleHeaders(withExtra)
	got := string(reorderHeadBlock(buildBlock(first, shuffled)))

	if !strings.HasSuffix(got, "\r\n\r\n") {
		t.Fatalf("头块结尾异常:\n%s", got)
	}
	if !strings.Contains(got, "X-Another: 2\r\nX-Brand-New: 1") {
		t.Errorf("表外俩头的相对顺序应与输入一致:\n%s", got)
	}
	if !strings.Contains(got, "X-Brand-New: 1\r\n\r\n") {
		t.Errorf("表外头应追加在末尾:\n%s", got)
	}
	if !strings.HasPrefix(got, "GET /v3/config HTTP/1.1\r\nAccept: ") {
		t.Errorf("表内头顺序被破坏:\n%s", got)
	}
}

// TestReorderHeadBlockNoFabricatedHeaders 缺 Host/Content-Length 时不凭空造。
func TestReorderHeadBlockNoFabricatedHeaders(t *testing.T) {
	in := buildBlock("POST /v2/chat/completions HTTP/1.1", []string{"Accept: application/json"})
	got := string(reorderHeadBlock(in))
	for _, name := range []string{"Host:", "Content-Length:", "Connection:", "Accept-Encoding:"} {
		if strings.Contains(got, name) {
			t.Errorf("不该凭空造出 %s:\n%s", name, got)
		}
	}
	if !strings.Contains(got, "Accept: application/json") {
		t.Errorf("原有头丢了:\n%s", got)
	}
}

// TestReorderHeadBlockMultiLineValue 值含多行（obsolete folding）的头不被拆坏。
func TestReorderHeadBlockMultiLineValue(t *testing.T) {
	in := "POST /v2/chat/completions HTTP/1.1\r\n" +
		"Accept: application/json\r\n" +
		"X-Folded: first\r\n" +
		"\tsecond\r\n" +
		"X-After: v\r\n\r\n"
	got := string(reorderHeadBlock([]byte(in)))
	if !strings.Contains(got, "X-Folded: first\r\n\tsecond\r\n") {
		t.Errorf("多行头的续行被拆坏了:\n%s", got)
	}
	if !strings.Contains(got, "X-After: v") {
		t.Errorf("续行吞掉了后面的头:\n%s", got)
	}
}

// TestReorderHeadBlockValueWithCRLF 值里带 \\r\\n 的头不破坏整体解析。
func TestReorderHeadBlockValueWithCRLF(t *testing.T) {
	// 值里含裸 \r\n 的畸形头：应保持字节不丢、不 panic
	in := "POST /v2/chat/completions HTTP/1.1\r\n" +
		"Accept: application/json\r\n" +
		"X-Bad: a\r\nb\r\n" +
		"X-After: v\r\n\r\n"
	got := reorderHeadBlock([]byte(in))
	if !bytes.Contains(got, []byte("X-Bad: a")) || !bytes.Contains(got, []byte("X-After: v")) {
		t.Errorf("畸形头的字节丢失:\n%s", got)
	}
}

// TestReorderHeadBlockMalformed 畸形输入不 panic、不破坏。
func TestReorderHeadBlockMalformed(t *testing.T) {
	cases := []string{
		"",
		"POST /v2/chat/completions HTTP/1.1\r\n", // 无结尾空行
		"garbage",
		"\r\n\r\n",
		"garbage\r\n\r\n",
		"NOTHTTP\r\nX-A: 1\r\n\r\n",
	}
	for _, in := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("输入 %q panic: %v", in, r)
				}
			}()
			out := reorderHeadBlock([]byte(in))
			if in != "garbage\r\n\r\n" && !strings.HasPrefix(in, "NOTHTTP") && string(out) != in {
				t.Errorf("输入 %q 被改写为 %q", in, out)
			}
		}()
	}
}

// TestReorderHeadBlockNonHTTPPassthrough 非 HTTP 字节原样透传。
func TestReorderHeadBlockNonHTTPPassthrough(t *testing.T) {
	in := []byte("garbage\r\n\r\n")
	if got := reorderHeadBlock(in); !bytes.Equal(got, in) {
		t.Errorf("非 HTTP 字节应原样透传: %q", got)
	}
}

// TestHeadOrderForPath 表外路径返回 nil。
func TestHeadOrderForPath(t *testing.T) {
	if o := headOrderForPath("/v2/chat/completions"); len(o) == 0 {
		t.Fatal("chat 路径应有顺序表")
	}
	if o := headOrderForPath("/v9/unknown"); o != nil {
		t.Errorf("未知路径应返回 nil: %v", o)
	}
}

// newTestPipe 用 net.Pipe 抽出 headOrderConn 的写侧行为。
func newTestPipe(t *testing.T) (*headOrderConn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return &headOrderConn{Conn: a, order: headOrderForPath}, b
}

// TestHeadOrderConnWriteReorders 一次写完整头块：对端应收到官方顺序。
func TestHeadOrderConnWriteReorders(t *testing.T) {
	c, peer := newTestPipe(t)
	first, headers := mustFixture(t, fixtureV3ConfigHead)
	in := buildBlock(first, shuffleHeaders(headers))

	var wg sync.WaitGroup
	wg.Add(1)
	var got []byte
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		n, _ := peer.Read(buf)
		got = buf[:n]
	}()
	if _, err := c.Write(in); err != nil {
		t.Fatalf("write: %v", err)
	}
	wg.Wait()
	if !strings.HasPrefix(string(got), "GET /v3/config HTTP/1.1\r\nAccept: ") {
		t.Errorf("写侧未重排:\n%s", got)
	}
}

// TestHeadOrderConnWriteSplitAcrossCalls 头块分两次写也能重排。
func TestHeadOrderConnWriteSplitAcrossCalls(t *testing.T) {
	c, peer := newTestPipe(t)
	first, headers := mustFixture(t, fixtureV3ConfigHead)
	in := buildBlock(first, shuffleHeaders(headers))
	cut := len(in) / 2

	var wg sync.WaitGroup
	wg.Add(1)
	var got []byte
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		n, _ := peer.Read(buf)
		got = append(got, buf[:n]...)
	}()

	if _, err := c.Write(in[:cut]); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if _, err := c.Write(in[cut:]); err != nil {
		t.Fatalf("write2: %v", err)
	}
	wg.Wait()
	if !strings.HasPrefix(string(got), "GET /v3/config HTTP/1.1\r\nAccept: ") {
		t.Errorf("分块写未重排:\n%s", got)
	}
}

// TestHeadOrderConnNonHTTPPassthrough 非 HTTP 字节原样透传。
func TestHeadOrderConnNonHTTPPassthrough(t *testing.T) {
	c, peer := newTestPipe(t)
	in := []byte("garbage\r\n\r\n")

	var wg sync.WaitGroup
	wg.Add(1)
	var got []byte
	go func() {
		defer wg.Done()
		buf := make([]byte, 64)
		n, _ := peer.Read(buf)
		got = buf[:n]
	}()
	if _, err := c.Write(in); err != nil {
		t.Fatalf("write: %v", err)
	}
	wg.Wait()
	if !bytes.Equal(got, in) {
		t.Errorf("非 HTTP 字节应原样透传: %q", got)
	}
}

// TestReorderPreservesBody 头块之后的数据必须原样透传（不能吞 body）。
func TestReorderPreservesBody(t *testing.T) {
	c, peer := newTestPipe(t)
	first, headers := mustFixture(t, fixtureV3ConfigHead)
	head := buildBlock(first, shuffleHeaders(headers))
	body := []byte("{\"k\":\"v\"}")
	in := append(append([]byte{}, head...), body...)

	var wg sync.WaitGroup
	wg.Add(1)
	var got []byte
	go func() {
		defer wg.Done()
		buf := make([]byte, 8192)
		for {
			n, err := peer.Read(buf)
			if n > 0 {
				got = append(got, buf[:n]...)
			}
			if err != nil || bytes.Contains(got, body) {
				return
			}
		}
	}()
	c.Write(in)
	wg.Wait()
	if !bytes.HasSuffix(got, body) {
		t.Errorf("body 丢了或被改: %q", got)
	}
}

// TestRespTrackerContentLength 有 Content-Length 时按字节数判断结束。
func TestRespTrackerContentLength(t *testing.T) {
	var tr respTracker
	if tr.feed([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n")) {
		t.Fatal("body 未到不该结束")
	}
	if tr.feed([]byte("abc")) {
		t.Fatal("body 未满不该结束")
	}
	if !tr.feed([]byte("de")) {
		t.Fatal("body 满应结束")
	}
}

// TestRespTrackerChunked chunked 按分帧结束判定。
func TestRespTrackerChunked(t *testing.T) {
	var tr respTracker
	head := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	if tr.feed([]byte(head + "3\r\nabc\r\n")) {
		t.Fatal("chunked 未完不该结束")
	}
	if !tr.feed([]byte("0\r\n\r\n")) {
		t.Fatal("结束块应判定完成")
	}
}

// TestRespTrackerUnknownFallsBack 无长度无 chunked → 标记 unknown，永不误判完成。
func TestRespTrackerUnknownFallsBack(t *testing.T) {
	var tr respTracker
	if tr.feed([]byte("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nsome body")) {
		t.Fatal("无边界信息时不该判定完成")
	}
	if !tr.unknown {
		t.Fatal("应标记 unknown 走安全降级")
	}
	if tr.feed([]byte("more")) {
		t.Fatal("unknown 之后不该再判定完成")
	}
}
