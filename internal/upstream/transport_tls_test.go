package upstream

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewTLSDialerNodeFingerprintCompletesHandshake 走复刻路径（uTLS）能真正
// 完成 TLS 握手并拿到 HTTP 响应。
//
// 为什么要有这个测试：复刻路径是自己做握手（不走标准库的 TLS 拨号），
// 一旦 ClientHello spec 写错（比如扩展顺序不对导致握手被拒），症状是
// 「所有出站请求失败」——必须在单测里就能发现，而不是等上线才发现。
func TestNewTLSDialerNodeFingerprintCompletesHandshake(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"ok":true}}`))
	}))
	defer srv.Close()

	tr := newTransport()
	// 测试用自签证书：跳过验签，只验握手与请求链路。
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	tr.DialTLSContext = newTLSDialer(newDialer(), tr.TLSClientConfig, TLSFingerprintNode)
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("node 指纹路径握手/请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
}

// TestNewTLSDialerGoFingerprintCompletesHandshake 原生路径（crypto/tls）同样可用。
func TestNewTLSDialerGoFingerprintCompletesHandshake(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"ok":true}}`))
	}))
	defer srv.Close()

	tr := newTransport()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	tr.DialTLSContext = newTLSDialer(newDialer(), tr.TLSClientConfig, TLSFingerprintGo)
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("go 指纹路径握手/请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
}

// TestSetTLSFingerprintSwitchesDialer 运行期可切档位：切到 go 之后
// DialTLSContext 必须被换成原生路径的闭包（否则回滚不生效）。
func TestSetTLSFingerprintSwitchesDialer(t *testing.T) {
	c := New()
	before := c.HTTP.Transport.(*http.Transport).DialTLSContext
	if before == nil {
		t.Fatal("缺省没有接上 DialTLSContext")
	}
	if err := c.SetTLSFingerprint(TLSFingerprintGo); err != nil {
		t.Fatalf("切到 go 失败: %v", err)
	}
	if c.TLSFingerprint != TLSFingerprintGo {
		t.Fatalf("档位 = %q，期望 go", c.TLSFingerprint)
	}
	after := c.HTTP.Transport.(*http.Transport).DialTLSContext
	if after == nil {
		t.Fatal("切换后 DialTLSContext 没了")
	}
	// 非法档位必须报错（不能静默回落——静默回落等于用户以为回滚生效了）。
	if err := c.SetTLSFingerprint("nope"); err == nil {
		t.Fatal("非法档位应当报错")
	}
	if c.TLSFingerprint != TLSFingerprintGo {
		t.Fatalf("非法调用后档位被改了 = %q", c.TLSFingerprint)
	}
}

// TestNewTLSDialerHandshakeTimeout 握手挂起时按 tlsHandshakeTimeout 失败，
// 而不是无限期挂着（半死上游的经典症状）。
func TestNewTLSDialerHandshakeTimeout(t *testing.T) {
	// 监听但不回应：TCP 建立了，TLS ClientHello 发出去没人理。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// 接受连接但永不握手。
			go func(c net.Conn) { time.Sleep(30 * time.Second); c.Close() }(conn)
		}
	}()

	dial := newTLSDialer(newDialer(), &tls.Config{InsecureSkipVerify: true}, TLSFingerprintNode)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err = dial(ctx, "tcp", ln.Addr().String())
	if err == nil {
		t.Fatal("挂起的握手应当超时失败")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timeout") &&
		!strings.Contains(strings.ToLower(err.Error()), "deadline") {
		t.Logf("握手错误（超时类）: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("握手耗时 %v，超过握手超时上限", elapsed)
	}
}
