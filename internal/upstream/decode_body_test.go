package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"

	"github.com/andybalholm/brotli"
)

// gzipBytes 造一个 gzip 压缩体。
func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// zlibBytes 造一个 zlib 包装的 deflate 体。
func zlibBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

// rawDeflateBytes 造一个裸 deflate（无 zlib 头）体。
func rawDeflateBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate new: %v", err)
	}
	if _, err := fw.Write([]byte(s)); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

// brBytes 造一个 brotli 压缩体。
func brBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write([]byte(s)); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeResponseBody(t *testing.T) {
	cases := []struct {
		name string
		enc  string
		body []byte
		want string
	}{
		{"无编码", "", []byte(`{"code":0}`), `{"code":0}`},
		{"identity", "identity", []byte(`{"code":0}`), `{"code":0}`},
		{"gzip", "gzip", gzipBytes(t, `{"code":0}`), `{"code":0}`},
		{"x-gzip", "x-gzip", gzipBytes(t, `{"code":0}`), `{"code":0}`},
		{"deflate-zlib", "deflate", zlibBytes(t, `{"code":0}`), `{"code":0}`},
		{"deflate-裸", "deflate", rawDeflateBytes(t, `{"code":0}`), `{"code":0}`},
		{"br", "br", brBytes(t, `{"code":0}`), `{"code":0}`},
		{"大小写不敏感", "GZIP", gzipBytes(t, `{"code":0}`), `{"code":0}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tc.enc != "" {
				resp.Header.Set("Content-Encoding", tc.enc)
			}
			got, err := decodeResponseBody(resp, tc.body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeResponseBodyMultiEncoding(t *testing.T) {
	// "gzip, br" 表示内容先被 gzip 再被 br 包过；逆序解：先解 br，再解 gzip。
	inner := gzipBytes(t, `{"code":0}`)
	outer := brBytes(t, string(inner))
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Encoding", "gzip, br")
	got, err := decodeResponseBody(resp, outer)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got) != `{"code":0}` {
		t.Fatalf("got %q", got)
	}
}

func TestDecodeResponseBodyUnknownEncoding(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Encoding", "weird")
	if _, err := decodeResponseBody(resp, []byte("x")); err == nil {
		t.Fatal("期望不支持编码报错")
	} else if !strings.Contains(err.Error(), "weird") {
		t.Fatalf("错误信息未含编码名: %v", err)
	}
}

func TestWrapDecodedBody(t *testing.T) {
	cases := []struct {
		name string
		enc  string
		body []byte
	}{
		{"gzip", "gzip", gzipBytes(t, `{"code":0,"msg":"ok"}`)},
		{"br", "br", brBytes(t, `{"code":0,"msg":"ok"}`)},
		{"deflate", "deflate", zlibBytes(t, `{"code":0,"msg":"ok"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: 200,
				Header:     http.Header{},
				Body:       io.NopCloser(bytes.NewReader(tc.body)),
			}
			resp.Header.Set("Content-Encoding", tc.enc)
			wrapDecodedBody(resp)
			if resp.Header.Get("Content-Encoding") != "" {
				t.Fatal("Content-Encoding 应被摘掉，避免下游二次误判")
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var env struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("unmarshal %q: %v", raw, err)
			}
			if env.Code != 0 || env.Msg != "ok" {
				t.Fatalf("got %+v", env)
			}
		})
	}
}

// TestDoJSONDecodesCompressedBody 端到端：上游回 br 压缩体，网关必须能解出信封。
func TestDoJSONDecodesCompressedBody(t *testing.T) {
	payload := `{"code":0,"data":{"ok":true}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(200)
		_, _ = w.Write(brBytes(t, payload))
	}))
	defer srv.Close()

	c := New()
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	c.CommonHeaders(req, nil)
	data, err := c.doJSON(req)
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("got %q", data)
	}
}

// TestDoJSONDecodesCompressedErrorBody 400 + 压缩体：Classify 必须看到明文。
func TestDoJSONDecodesCompressedErrorBody(t *testing.T) {
	body := `{"code":6021,"msg":"queue full"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(429)
		_, _ = w.Write(gzipBytes(t, body))
	}))
	defer srv.Close()

	c := New()
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	c.CommonHeaders(req, nil)
	_, err = c.doJSON(req)
	if err == nil {
		t.Fatal("期望报错")
	}
	upErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("期望 *Error，实际 %T: %v", err, err)
	}
	if upErr.Kind != ErrUpstreamBusy {
		t.Fatalf("kind=%v 期望 ErrUpstreamBusy", upErr.Kind)
	}
	if !strings.Contains(upErr.Msg, "queue full") {
		t.Fatalf("错误信息应含明文，实际 %q", upErr.Msg)
	}
}

// TestCommonHeadersAcceptEncoding 断言官方四段声明已就位。
func TestCommonHeadersAcceptEncoding(t *testing.T) {
	c := New()
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	c.CommonHeaders(req, nil)
	if got := req.Header.Get("Accept-Encoding"); got != "gzip, compress, deflate, br" {
		t.Fatalf("Accept-Encoding=%q", got)
	}
}

// TestChatHeadersDropsAcceptEncoding chat 流式交回 net/http 自动 gzip。
func TestChatHeadersDropsAcceptEncoding(t *testing.T) {
	c := New()
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, &auth.Auth{}, "", ChatMeta{})
	if got := req.Header.Get("Accept-Encoding"); got != "" {
		t.Fatalf("chat 不应自带 Accept-Encoding，实际 %q", got)
	}
}
