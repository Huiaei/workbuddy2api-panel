package upstream

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// headerFold 把 header 折叠成小写键，便于按 wire 上的真实拼写断言。
func headerFold(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

func TestNormalizeHeaderSpellingKeepsOfficialWireCase(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Go 的 Set 会把大小写混合的头名拍成 canonical（X-Conversation-Id）。
	req.Header.Set("X-Conversation-ID", "conv-1")
	req.Header.Set("X-Request-ID", "req-1")
	req.Header.Set("X-B3-ParentSpanId", "span-1")
	req.Header.Set("User-Agent", "WorkBuddy/5.9.3")

	got := normalizeHeaderSpelling(req)
	for _, want := range []string{"X-Conversation-ID", "X-Request-ID", "X-B3-ParentSpanId", "User-Agent"} {
		if _, ok := got.Header[want]; !ok {
			t.Errorf("归一化后缺少官方拼写 %q；实际键：%v", want, headerKeys(got.Header))
		}
	}
	// 原请求不能被就地改写（调用方回读 Header.Get 时会取不到值）。
	if _, ok := req.Header["X-Conversation-ID"]; ok {
		t.Error("normalizeHeaderSpelling 原地改了原请求的 header map")
	}
}

func TestNormalizeHeaderSpellingDoesNotLowercaseSpecialHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "WorkBuddy/5.9.3 CLI/2.165.0")
	req.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")

	got := normalizeHeaderSpelling(req)
	if v := got.Header.Get("User-Agent"); v != "WorkBuddy/5.9.3 CLI/2.165.0" {
		t.Errorf("User-Agent 归一化后取不到：%q", v)
	}
	if v := got.Header.Get("Accept-Encoding"); v != "gzip, compress, deflate, br" {
		t.Errorf("Accept-Encoding 归一化后取不到：%q", v)
	}
}

func TestNormalizeHeaderSpellingKeepsUnknownHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://www.workbuddy.cn/v2/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Enterprise-Id", "ent-1")
	got := normalizeHeaderSpelling(req)
	if v := got.Header.Get("X-Enterprise-Id"); v != "ent-1" {
		t.Errorf("表外头被丢掉：%q", v)
	}
}

func TestOfficialConnectionValue(t *testing.T) {
	cases := map[string]string{
		"/v3/config":            "close",
		"/v2/chat/completions":  "keep-alive",
		"/v2/chat/busy":         "keep-alive",
		"/v2/report":            "keep-alive",
		"/v2/plugin/auth/state": "",
	}
	for path, want := range cases {
		if got := officialConnectionValue(path); got != want {
			t.Errorf("officialConnectionValue(%q)=%q，期望 %q", path, got, want)
		}
	}
}

func TestApplyConnectionHeaderKeepsAlive(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyConnectionHeader(req)
	if v := req.Header.Get("Connection"); v != "keep-alive" {
		t.Errorf("keep-alive 分支未写 Connection 头：%q", v)
	}
	if req.Close {
		t.Error("keep-alive 分支不应设置 req.Close")
	}
}

func TestApplyConnectionHeaderCloseUsesReqClose(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v3/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyConnectionHeader(req)
	if !req.Close {
		t.Error("close 分支未设置 req.Close")
	}
	// 手写 Connection 会让 net/http 写出两行 Connection，故必须不写进 Header。
	if v := req.Header.Get("Connection"); v != "" {
		t.Errorf("close 分支不该手写 Connection 头：%q", v)
	}
}

// TestDoSendsOfficialHeaderSpellingAndConnection 端到端：经 do() 发出的请求，
// 在 wire 上抓到的头名拼写与 Connection 值都要与官方真机一致。
//
// 为什么用裸 TCP 抓原文而不是 httptest：服务端解析后 header key 会被
// CanonicalMIMEHeaderKey 归一化（X-Conversation-ID → X-Conversation-Id），
// 那样就看不出 wire 上的真实拼写了——本改动要对齐的正是 wire 字节。
func TestDoSendsOfficialHeaderSpellingAndConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	rawCh := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 8192)
		n, _ := conn.Read(buf)
		rawCh <- string(buf[:n])
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}"))
	}()

	c := New()
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v2/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Conversation-ID", "conv-1")
	req.Header.Set("X-Request-ID", "req-1")
	req.Header.Set("User-Agent", "WorkBuddy/5.9.3 CLI/2.165.0")

	resp, err := c.do(req, false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := <-rawCh

	for _, want := range []string{"X-Conversation-ID: conv-1", "X-Request-ID: req-1", "User-Agent: WorkBuddy/5.9.3 CLI/2.165.0"} {
		if !strings.Contains(raw, want) {
			t.Errorf("wire 上缺少 %q；原文：\n%s", want, raw)
		}
	}
	if !strings.Contains(raw, "Connection: keep-alive") {
		t.Errorf("wire 上缺少 Connection: keep-alive；原文：\n%s", raw)
	}
	// Go canonical 拼写不应出现（那正是本改动要消掉的痕迹）。
	if strings.Contains(raw, "X-Conversation-Id:") {
		t.Errorf("wire 上仍是 Go canonical 拼写 X-Conversation-Id；原文：\n%s", raw)
	}
}

// TestDoDoesNotBreakChatStreaming 回归：do() 走 chat 通道时不影响流式读。
func TestDoDoesNotBreakChatStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"x\":1}\n\n")
	}))
	defer srv.Close()

	c := New()
	c.ChatHTTP = srv.Client()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.do(req, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"x":1`) {
		t.Errorf("流式响应体异常：%q", string(raw))
	}
}

func headerKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return keys
}
