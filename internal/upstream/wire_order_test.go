package upstream

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// selfSignedTLSServer 起一个「能看到明文请求头」的 TLS 服务器。
//
// 为什么不能用 httptest.NewTLSServer：它在 handler 里看到的 header 已经被
// 标准库按 canonical MIME 规则归一化（X-Conversation-ID → X-Conversation-Id），
// 而且顺序也被重排过——正好把我们要验证的两件事（wire 上的真实拼写与顺序）
// 都抹掉了。所以这里自己起 TLS：tls.Server 握手之后从它读出来的就是
// 客户端发来的**明文 HTTP 请求头字节**，与线上的字节序列一致。
func selfSignedTLSServer(t *testing.T, handle func(wireHead string)) (*http.Transport, string) {
	t.Helper()
	cert := genSelfSignedCert(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := tc.Handshake(); err != nil {
					c.Close()
					return
				}
				br := bufio.NewReader(tc)
				var sb strings.Builder
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						break
					}
					sb.WriteString(line)
					if line == "\r\n" {
						break
					}
				}
				handle(sb.String())
				// 回一个最小合法响应，客户端才能正常结束。
				body := `{"code":0,"data":{}}`
				fmt.Fprintf(tc, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
				tc.Close()
			}(conn)
		}
	}()

	tr := newTransport()
	tr.DialTLSContext = newTLSDialer(newDialer(), &tls.Config{InsecureSkipVerify: true}, TLSFingerprintNode)
	return tr, "https://" + ln.Addr().String()
}

// genSelfSignedCert 生成一张自签证书（测试用）。
func genSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("序列化私钥失败: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("组装密钥对失败: %v", err)
	}
	return pair
}

// TestWireHeaderOrderThroughRealTransport 走真实出站链路（c.do：官方拼写 +
// Connection 头 + uTLS 指纹 + 头顺序重排）打一次 chat 请求，断言服务器收到的
// 明文请求头：
//  1. 顺序与官方真机表一致（X-Conversation-ID 在 X-Request-ID 之前等）；
//  2. 官方拼写没有被 Go 的 canonical 化改写（wire 上是 X-Conversation-ID，
//     不是 X-Conversation-Id）；
//  3. Connection 头存在且排在 Host 之后。
//
// 这是「出站头对齐」三件套（拼写 headcase.go / 顺序 headorder.go /
// Connection send.go）唯一的端到端守护：任何一件被改坏，这里立刻红。
//
// 走 c.do 而不是裸 http.Client：拼写归一与 Connection 注入都发生在 c.do 里，
// 裸 client 绕过它们，测的就不是真实出站字节了。
func TestWireHeaderOrderThroughRealTransport(t *testing.T) {
	headCh := make(chan string, 4)
	tr, base := selfSignedTLSServer(t, func(wire string) { headCh <- wire })

	c := &Client{
		HTTP:     &http.Client{Transport: tr, Timeout: 10 * time.Second},
		ChatHTTP: &http.Client{Transport: tr, Timeout: 0},
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v2/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("X-Conversation-ID", "conv-wire-1")
	req.Header.Set("X-Request-ID", "req-wire-1")
	req.Header.Set("X-Conversation-Message-ID", "msg-wire-1")
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0")

	resp, err := c.do(req, true)
	if err != nil {
		t.Fatalf("出站失败: %v", err)
	}
	resp.Body.Close()

	var wire string
	select {
	case wire = <-headCh:
	case <-time.After(5 * time.Second):
		t.Fatal("没有收到服务器侧请求头")
	}

	// 官方拼写：wire 上必须是 X-Conversation-ID（不是 X-Conversation-Id）。
	if !strings.Contains(wire, "X-Conversation-ID: conv-wire-1") {
		t.Errorf("wire 上没有官方拼写的 X-Conversation-ID:\n%s", wire)
	}
	if !strings.Contains(wire, "X-Request-ID: req-wire-1") {
		t.Errorf("wire 上没有官方拼写的 X-Request-ID:\n%s", wire)
	}
	// 顺序判别（关键）：官方 chat 头序里 X-Request-ID 在 X-Conversation-Message-ID
	// **之前**，而 Go 的 canonical 字母序恰好相反（X-Conversation-* < X-Request-*）。
	// 所以这一对能真正区分「按官方表重排」与「只是字母序」。
	iReqFirst := strings.Index(wire, "X-Request-ID:")
	iMsg := strings.Index(wire, "X-Conversation-Message-ID:")
	if iReqFirst < 0 || iMsg < 0 || iReqFirst > iMsg {
		t.Errorf("官方顺序不对：X-Request-ID@%d 应在 X-Conversation-Message-ID@%d 之前（说明没按官方表重排）\n%s", iReqFirst, iMsg, wire)
	}
	// 顺序：X-Conversation-ID 在 X-Request-ID 之前（官方 chat 头序）。
	iConv := strings.Index(wire, "X-Conversation-ID:")
	iReq := strings.Index(wire, "X-Request-ID:")
	if iConv < 0 || iReq < 0 || iConv > iReq {
		t.Errorf("头顺序不对：X-Conversation-ID@%d 应该在 X-Request-ID@%d 之前\n%s", iConv, iReq, wire)
	}
	// Connection：官方真机有，且排在 Host 之后。
	if !strings.Contains(wire, "Connection: keep-alive") {
		t.Errorf("wire 上没有 Connection: keep-alive:\n%s", wire)
	}
	iHost := strings.Index(wire, "Host:")
	iConn := strings.Index(wire, "Connection:")
	if iHost < 0 || iConn < 0 || iConn < iHost {
		t.Errorf("Connection 头应在 Host 之后（Host@%d, Connection@%d）\n%s", iHost, iConn, wire)
	}
}
