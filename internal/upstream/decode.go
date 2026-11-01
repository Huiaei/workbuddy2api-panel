// decode.go 出站响应体的按需解压。
//
// 为什么需要自己解：一旦请求显式声明 Accept-Encoding，net/http 就**关闭透明
// 解压**（Transport 只在「自己加的 gzip」时才解）。不解这一层，doJSON 拿到
// brotli/gzip 字节直接 json.Unmarshal → 所有 RPC 链路全废，而且 400 分支的
// Classify 会把压缩体当文本分类，罚号逻辑跟着错。
//
// 挂载点选在 do()（唯一出站出口）而不是每个读 body 的调用点：do() 之后所有
// 读法（doJSON / 个人模型列表 / v3-config 探测 / search / global_models）都
// 直接吃 resp.Body，包一层就一次覆盖全部，且未来新增出站点自动受益。
//
// 判据：net/http 自动解压成功时会**删掉** Content-Encoding 头并置
// resp.Uncompressed=true；只有「我们自己声明了 Accept-Encoding」时该头仍在。
// 故这里以「头还在」为解压依据，不会对已解压的响应二次解压。
package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

// officialAcceptEncoding 真机全链路同值的四段声明。
//
// 真机 chat / report / billing / refresh 的 Accept-Encoding 逐条统计均为
// `gzip, compress, deflate, br`（billing 21/21、report 1/1 命中）。
// `compress`(LZW) 官方虽声明但服务端从不回，解码时明确报错而非返回乱码。
const officialAcceptEncoding = "gzip, compress, deflate, br"

// decodeResponseBody 按 Content-Encoding 解压响应体（多段编码按逆序解）。
func decodeResponseBody(resp *http.Response, raw []byte) ([]byte, error) {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" || len(raw) == 0 {
		return raw, nil
	}
	parts := strings.Split(enc, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		e := strings.TrimSpace(parts[i])
		var err error
		switch e {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			raw, err = io.ReadAll(gzipReader(bytes.NewReader(raw)))
		case "deflate":
			// RFC 7230 的 deflate 实际可能是 zlib 包装或裸 deflate，两种都试。
			raw, err = inflate(raw)
		case "br":
			raw, err = io.ReadAll(brotli.NewReader(bytes.NewReader(raw)))
		default:
			return nil, fmt.Errorf("不支持的内容编码 %q（响应头 Content-Encoding）", e)
		}
		if err != nil {
			return nil, fmt.Errorf("解压 %s 失败: %w", e, err)
		}
	}
	return raw, nil
}

// gzipReader 用标准库建 gzip 读取器（封一层便于统一错误处理）。
func gzipReader(r io.Reader) io.Reader {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return errReader{err}
	}
	return zr
}

// inflate 解 deflate：先按 zlib 试，失败再按裸 deflate 试。
func inflate(raw []byte) ([]byte, error) {
	if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
		defer zr.Close()
		if out, err := io.ReadAll(zr); err == nil {
			return out, nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(raw))
	defer fr.Close()
	return io.ReadAll(fr)
}

// errReader 把构造期的错误推迟到 Read 时返回。
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// decodedBody 是包在 resp.Body 外的一层解压读端（支持 gzip/deflate/br 与多段）。
type decodedBody struct {
	src io.ReadCloser
	rd  io.Reader
}

func (d *decodedBody) Read(p []byte) (int, error) { return d.rd.Read(p) }

func (d *decodedBody) Close() error { return d.src.Close() }

// wrapDecodedBody 在响应带 Content-Encoding 时把 body 包成解压流。
//
// 已在 Header 上把 Content-Encoding 摘掉（解压后 body 不再是压缩态，留着会让
// 下游二次误判）。解码失败只能在读时暴露——用 errReader 承载。
func wrapDecodedBody(resp *http.Response) {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return
	}
	resp.Header.Del("Content-Encoding")
	// 多段编码（如 "gzip, br"）按逆序解：最后声明的先解。
	parts := strings.Split(enc, ",")
	var rd io.Reader = resp.Body
	for i := len(parts) - 1; i >= 0; i-- {
		e := strings.TrimSpace(parts[i])
		switch e {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			rd = gzipReader(rd)
		case "deflate":
			// 流式 deflate 无法「先试 zlib 再试裸」，故这里只走 zlib；
			// 裸 flate 的响应极罕见，真出现时由 doJSON 的 decodeResponseBody
			// 兜底（那条路径是整块读入的，能两种都试）。
			rd = zlibReader(rd)
		case "br":
			rd = brotli.NewReader(rd)
		default:
			rd = errReader{fmt.Errorf("不支持的内容编码 %q（响应头 Content-Encoding）", e)}
		}
	}
	resp.Body = &decodedBody{src: resp.Body, rd: rd}
}

// zlibReader 建 zlib 读取器（构造失败时返回会在 Read 时报错的 reader）。
func zlibReader(r io.Reader) io.Reader {
	zr, err := zlib.NewReader(r)
	if err != nil {
		return errReader{err}
	}
	return zr
}
