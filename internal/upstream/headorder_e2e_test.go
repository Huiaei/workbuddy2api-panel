// headorder_e2e_test.go 端到端验证：真实 TCP 连接上，Transport 写出的请求头
// 是否已按官方顺序排列。
//
// 为什么必须端到端：headorder_test.go 验的是纯函数和 conn 包装，但「Transport
// 到底有没有用我们替换的 DialContext」只有真跑一遍才能确认——这一层如果没接上，
// 前面所有测试全绿而线上毫无变化。
//
// 手段：裸 TCP 监听（不用 TLS，直接看 wire 原文），Transport 禁 h2，用
// DialContext 注入 headOrderConn；服务端用 conn.Read 抓 wire 原文。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawHeadServer 起一个裸 TCP 监听，抓取第一条请求的 wire 原文头块。
// 返回监听地址与读取到的头块字节。
func rawHeadServer(t *testing.T) (string, func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var raw []byte

	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				var head []byte
				for {
					ln, rerr := br.ReadBytes('\n')
					head = append(head, ln...)
					if rerr != nil {
						break
					}
					if string(ln) == "\r\n" {
						break
					}
				}
				mu.Lock()
				if raw == nil {
					raw = head
				}
				mu.Unlock()
				// 回一个最小 200：Content-Length 明确，便于 wrapper 复位
				c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Type: application/json\r\n\r\n{}"))
			}(conn)
		}
	}()

	return ln.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(raw)
	}
}

// headNamesOf 从 wire 原文里取出头名序列（按出现顺序）。
func headNamesOf(raw string) []string {
	var out []string
	for _, ln := range strings.Split(raw, "\r\n") {
		if ln == "" {
			continue
		}
		if i := strings.Index(ln, ":"); i > 0 {
			out = append(out, ln[:i])
		}
	}
	return out
}

// TestE2EHeadOrderOnWire 端到端：出站 wire 上的头顺序与官方真机一致。
func TestE2EHeadOrderOnWire(t *testing.T) {
	addr, getHead := rawHeadServer(t)

	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
			d := newDialer()
			c, err := d.DialContext(ctx, network, a)
			if err != nil {
				return nil, err
			}
			// 包在**握手之后**的连接上（这里是裸 TCP，本来就没握手）
			return newHeadOrderConnWithOrder(c, nil), nil
		},
		// 禁 h2：裸 TCP 无 TLS 也不会升级，但显式关掉更稳
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableCompression:    false,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	client := &http.Client{Transport: tr}

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v2/chat/completions",
		strings.NewReader(`{"model":"x","messages":[]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// 装一套接近官方的头（名字与真机一致；值不敏感）
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-stainless-arch", "x64")
	req.Header.Set("x-stainless-lang", "js")
	req.Header.Set("x-stainless-os", "Windows")
	req.Header.Set("x-stainless-package-version", "6.25.0")
	req.Header.Set("x-stainless-retry-count", "0")
	req.Header.Set("x-stainless-runtime", "node")
	req.Header.Set("x-stainless-runtime-version", "v22.21.1")
	req.Header.Set("X-Conversation-ID", "f7c6c392-e016-43bb-beca-ab918b436668")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Version", "5.9.3")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Runtime", "local-agent")
	req.Header.Set("X-Private-Data", "false")
	req.Header.Set("acp-connection-id", "01a194ad-6b5d-7394-92ad-61e04eb3a3df")
	req.Header.Set("x-codebuddy-request", "1")
	req.Header.Set("X-Request-ID", "01a194adc6bc7205b0d188e0c9b6a728")
	req.Header.Set("X-Conversation-Message-ID", "01a194adc6bc7205b0d188e0c9b6a728")
	req.Header.Set("X-Conversation-Request-ID", "01a194ad31c27d81804ee9915cf954fb")
	req.Header.Set("X-Root-Request-ID", "01a194ad31c27d81804ee9915cf954fb")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("baggage", "buddyId=,buddyName=")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("traceparent", "00-7458e488eb564b059622db42023ae293-82e31fea372dea0e-01")
	req.Header.Set("b3", "7458e488eb564b059622db42023ae293-82e31fea372dea0e-1-57587f4a3ce47317")
	req.Header.Set("X-B3-TraceId", "7458e488eb564b059622db42023ae293")
	req.Header.Set("X-B3-ParentSpanId", "57587f4a3ce47317")
	req.Header.Set("X-B3-SpanId", "82e31fea372dea0e")
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("X-Trace-ID", "7458e488eb564b059622db42023ae293")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0")
	req.Header.Set("Authorization", "Bearer REDACTED")
	req.Header.Set("X-User-Id", "11f49997-8875-4ce2-ae68-d093e3088524")
	req.Header.Set("X-Domain", "www.workbuddy.cn")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	raw := getHead()
	if raw == "" {
		t.Fatal("服务端没收到请求头")
	}
	gotNames := headNamesOf(raw)
	t.Logf("wire 头顺序: %v", gotNames)

	// 官方顺序的 rank（用内嵌真机夹具）
	_, official := mustFixture(t, fixtureChatHead)
	rank := map[string]int{}
	for i, h := range official {
		if j := strings.Index(h, ":"); j > 0 {
			rank[strings.ToLower(h[:j])] = i
		}
	}

	// 1) 表内头的相对顺序必须与官方一致
	lastRank := -1
	lastName := ""
	for _, h := range gotNames {
		r, ok := rank[strings.ToLower(h)]
		if !ok {
			continue
		}
		if r < lastRank {
			t.Fatalf("顺序偏离官方：%q(rank %d) 出现在 %q(rank %d) 之后\n实际顺序: %v",
				h, r, lastName, lastRank, gotNames)
		}
		lastRank, lastName = r, h
	}

	// 2) Host / Connection / Content-Length / Accept-Encoding 必须落在末尾
	//
	// 注意：Go 的 transport 默认 keep-alive，**不写 Connection 头**（只有
	// DisableKeepAlives 才会写 "Connection: close"）。因此断言的是「四者中
	// 实际出现的那些必须是 wire 上的最后几个」，且相对顺序与官方一致。
	// 官方真机序：Content-Length → Accept-Encoding → Host → Connection。
	tailWant := []string{"content-length", "accept-encoding", "host", "connection"}
	var present []string
	for _, w := range tailWant {
		for _, h := range gotNames {
			if strings.EqualFold(h, w) {
				present = append(present, w)
				break
			}
		}
	}
	if len(present) == 0 {
		t.Fatalf("尾部四头一个都没出现: %v", gotNames)
	}
	tail := gotNames[len(gotNames)-len(present):]
	for i, h := range tail {
		if !strings.EqualFold(h, present[i]) {
			t.Errorf("末尾 %d 个头应为 %v，实际 %v（全量: %v）", len(present), present, tail, gotNames)
			break
		}
	}
	for i, h := range gotNames[:len(gotNames)-len(present)] {
		switch strings.ToLower(h) {
		case "host", "connection", "content-length", "accept-encoding":
			t.Errorf("%q 出现在非末尾位置(%d): %v", h, i, gotNames)
		}
	}
}

// TestE2ENonHTTPPassthrough 失败安全：非 HTTP 字节原样透传（conn 层）。
func TestE2ENonHTTPPassthrough(t *testing.T) {
	c, peer := newTestPipe(t)
	in := []byte("garbage\r\n\r\n")
	var got []byte
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		n, _ := peer.Read(buf)
		got = buf[:n]
		close(done)
	}()
	c.Write(in)
	<-done
	if !bytes.Equal(got, in) {
		t.Errorf("非 HTTP 字节应原样透传: %q", got)
	}
}
