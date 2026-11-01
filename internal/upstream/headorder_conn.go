// headorder_conn.go 把官方头顺序落到网络层。
//
// 为什么必须在 conn 层做：Go 的 net/http 自己序列化请求头（Header.writeSubset
// 按 key 排序），RoundTripper 拿不到「按顺序写字节」的控制权，除非把整个
// HTTP/1.1 客户端重写一遍。包一层 net.Conn、在 Write 里重排头块，是唯一
// 既保留 Transport 全部能力（连接池 / TLS / 超时 / 半死连接清理）又能改顺序
// 的位置。
//
// **失败安全红线**：本文件所有解析都是 best-effort。任何一步判断不出来就原样
// 透传——最坏结果只是「没重排」（退回字母序），绝不改变数据流本身。
//
// **接线注意**：uTLS / crypto-tls 的连接必须在**握手之后**再包
// （tls.Conn 写出的字节是密文，重排无从谈起）：
//
//	tlsConn := tls.Client(raw, cfg)      // 握手
//	_ = tlsConn.Handshake()
//	ordered := upstream.NewHeadOrderConn(tlsConn, nil) // 之后再包
package upstream

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"sync"
)

// connPhase 一条连接上的当前位置。
type connPhase int

const (
	phaseAwaitRequest connPhase = iota // 下一个字节开始是请求头
	phaseInBody                        // 请求体/响应体流动中
)

// maxPendingHead 请求头累积上限：超过就放弃重排并原样放行。
// 官方头块约 1.8 KB，正常远达不到；这是防「头异常大 / 判断不出边界」时
// 无限缓冲吃内存。
const maxPendingHead = 64 * 1024

// headOrderConn 在 Write 侧按官方顺序重排请求头块，Read 侧只做解析不改数据。
type headOrderConn struct {
	net.Conn

	mu      sync.Mutex
	phase   connPhase
	pending []byte // 请求头累积（仅在 phaseAwaitRequest 有意义）

	// order 决定按 path 取哪张顺序表；nil 表示用内置 officialHeadOrder。
	order func(path string) []string

	// Read 侧响应解析状态（只用于判断「响应体何时结束」→ 回到 phaseAwaitRequest）
	resp   respTracker
	respMu sync.Mutex
}

// NewHeadOrderConn 包装一条**已完成 TLS 握手**的连接，使其出站请求头按官方
// 顺序写出。
//
// order 为 nil 时使用内置 officialHeadOrder；非 nil 时由调用方决定顺序表
// （便于按域名/环境切换）。
//
// 注意：不要包在尚未握手的 tls.Conn 外面——那时 Write 出的是密文。
func NewHeadOrderConn(c net.Conn, order func(path string) []string) net.Conn {
	if order == nil {
		order = headOrderForPath
	}
	return &headOrderConn{Conn: c, order: order}
}

// newHeadOrderConn 包内构造函数：order 可选（省略或传 nil 时用内置顺序表）。
//
// 用可变参而非固定两参，是为了让接线侧既能写 newHeadOrderConn(c) 也能写
// newHeadOrderConn(c, customOrder)，两种调用都编译得过。
func newHeadOrderConn(c net.Conn, order ...func(path string) []string) net.Conn {
	var o func(path string) []string
	if len(order) > 0 {
		o = order[0]
	}
	return NewHeadOrderConn(c, o)
}

// newHeadOrderConnWithOrder 包内构造函数：显式自定义顺序表。
func newHeadOrderConnWithOrder(c net.Conn, order func(path string) []string) net.Conn {
	return NewHeadOrderConn(c, order)
}

// orderFor 取该 path 的顺序表（表外返回 nil → 原样透传）。
func (c *headOrderConn) orderFor(path string) []string {
	if c.order == nil {
		return headOrderForPath(path)
	}
	return c.order(path)
}

// Write 在请求头块完整时重排，其余情况原样透传。
func (c *headOrderConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.phase == phaseInBody {
		return c.Conn.Write(p)
	}

	c.pending = append(c.pending, p...)

	i := bytes.Index(c.pending, []byte("\r\n\r\n"))
	if i < 0 {
		// 头块未完整，继续等（调用方会继续 Write 或进入读阶段）
		if len(c.pending) > maxPendingHead {
			// 降级：原样放行已缓冲的字节，后续不再尝试重排
			out := c.pending
			c.pending = nil
			c.phase = phaseInBody
			if _, err := c.Conn.Write(out); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	}

	head := c.pending[:i+4]
	rest := c.pending[i+4:]
	c.pending = nil
	c.phase = phaseInBody

	out := head
	if path := blockRequestPath(head); path != "" {
		if order := c.orderFor(path); len(order) > 0 {
			out = reorderHeadBlockWithOrder(head, order)
		}
	}
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	if len(rest) > 0 {
		if _, err := c.Conn.Write(rest); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Read 只透传，同时把字节喂给响应解析器判断响应体边界。
func (c *headOrderConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.respMu.Lock()
		done := c.resp.feed(p[:n])
		c.respMu.Unlock()
		if done {
			c.mu.Lock()
			if c.phase == phaseInBody {
				c.phase = phaseAwaitRequest
				c.pending = nil
			}
			c.mu.Unlock()
		}
	}
	return n, err
}

// respTracker 跟踪一条 HTTP/1.1 响应，判断响应体何时读完。
//
// 只关心「边界」，不解析内容：响应头 → 依据 Content-Length / chunked /
// Connection: close 三种情形计数。任何不确定（字段畸形、多段编码）一律
// 当作「无法判断」→ 保持 phaseInBody（该连接后续不再重排，安全降级）。
type respTracker struct {
	inHead   bool // true = 正在收响应头
	headBuf  []byte
	remain   int64 // Content-Length 剩余
	chunked  bool
	chunkRem int64 // 当前 chunk 剩余
	chunkSt  int   // 0=等大小行 1=收数据 2=等 chunk 尾 CRLF 3=收 trailer
	unknown  bool  // 无法判断边界 → 永不回到 awaitRequest
	started  bool
}

// feed 喂入刚读到的字节，返回 true 表示「本响应已完整结束」。
func (t *respTracker) feed(b []byte) bool {
	if t.unknown {
		return false
	}
	if !t.started {
		t.started = true
		t.inHead = true
	}
	if t.inHead {
		t.headBuf = append(t.headBuf, b...)
		i := bytes.Index(t.headBuf, []byte("\r\n\r\n"))
		if i < 0 {
			if len(t.headBuf) > maxPendingHead {
				t.unknown = true
			}
			return false
		}
		headBytes := t.headBuf[:i]
		over := t.headBuf[i+4:] // 响应头之后已经读到的 body 字节
		t.headBuf = nil
		t.inHead = false

		lines := bytes.Split(headBytes, []byte("\r\n"))
		var clen int64 = -1
		for _, ln := range lines[1:] {
			l := string(ln)
			low := strings.ToLower(l)
			switch {
			case strings.HasPrefix(low, "content-length:"):
				clen = parseInt64(strings.TrimSpace(l[len("content-length:"):]))
				if clen < 0 {
					t.unknown = true
					return false
				}
			case strings.HasPrefix(low, "transfer-encoding:") && strings.Contains(low, "chunked"):
				t.chunked = true
			}
		}
		switch {
		case t.chunked:
			t.chunkSt = 0
		case clen >= 0:
			t.remain = clen
		default:
			// 无长度且非 chunked：以连接关闭为界，无法给出结束点
			t.unknown = true
			return false
		}
		if len(over) == 0 {
			return t.complete()
		}
		return t.feedBody(over)
	}
	return t.feedBody(b)
}

// feedBody 按已确定的分帧方式消费 body 字节。
func (t *respTracker) feedBody(b []byte) bool {
	if t.chunked {
		return t.feedChunked(b)
	}
	if int64(len(b)) >= t.remain {
		t.remain = 0
		return true
	}
	t.remain -= int64(len(b))
	return false
}

// feedChunked 解析 chunked 分帧直到结束块。
func (t *respTracker) feedChunked(b []byte) bool {
	i := 0
	for i < len(b) {
		switch t.chunkSt {
		case 0: // 等 chunk 大小行
			j := bytes.Index(b[i:], []byte("\r\n"))
			if j < 0 {
				return false // 半行，下次继续（行很短且极少跨包）
			}
			line := string(b[i : i+j])
			if k := strings.IndexByte(line, ';'); k >= 0 {
				line = line[:k]
			}
			n := parseInt64Hex(strings.TrimSpace(line))
			if n < 0 {
				t.unknown = true
				return false
			}
			i += j + 2
			if n == 0 {
				t.chunkSt = 3 // 末尾 trailer
				continue
			}
			t.chunkRem = n
			t.chunkSt = 1
		case 1: // 收 chunk 数据
			if int64(len(b)-i) < t.chunkRem {
				t.chunkRem -= int64(len(b) - i)
				return false
			}
			i += int(t.chunkRem)
			t.chunkRem = 0
			t.chunkSt = 2
		case 2: // 等 chunk 后 CRLF
			if len(b)-i < 2 {
				return false
			}
			i += 2
			t.chunkSt = 0
		case 3: // trailer 直到空行
			j := bytes.Index(b[i:], []byte("\r\n"))
			if j < 0 {
				return false
			}
			if j == 0 { // 空行 = 结束
				return true
			}
			i += j + 2
		}
	}
	return false
}

// complete 头到手但 body 为空时的判定。
func (t *respTracker) complete() bool {
	if t.chunked {
		return false // chunked 至少要有 0\r\n\r\n
	}
	return t.remain == 0
}

// parseInt64 解析十进制非负整数；失败返回 -1。
func parseInt64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// parseInt64Hex 解析十六进制非负整数；失败返回 -1。
func parseInt64Hex(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 16, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
