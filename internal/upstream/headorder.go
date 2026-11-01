// headorder.go 出站请求头的**发送顺序**对齐官方真机。
//
// 背景：Go 的 net/http 在写请求头时按 map key 排序（Header.writeSubset 的
// sortedKeyValues），而官方客户端是 Node/undici/axios，按**插入序**写出。
// 头名的集合与拼写已由 headcase.go 对齐，本文件补最后一项差异：顺序。
//
// 依据（真机夹具原文顺序，逐条抄录）：
//
//	/v2/chat/completions      → official_chat_head.txt（43 头）
//	/v2/report                 → official_report_cn_head.txt（11 头）
//	/v2/chat/busy              → official_chat_busy_head.txt（8 头）
//	/v3/config                 → official_v3config_head.txt（4 头）
//	/v2/plugin/auth/token/refresh → official_gl_refresh_head.txt（全局线）
//
// 顺序表按 **path** 分族：官方不同链路由不同代码构造头对象，顺序本就不通用，
// 拿 chat 的顺序套 report 反而更假。表里没有的路径保持 Go 原生排序——不做
// 无证据的猜测。
//
// 失败安全：本文件所有解析都是 best-effort。任何一步判断不出来就原样返回，
// 绝不改变数据流本身（最坏结果只是「没重排」，退回字母序）。
package upstream

import (
	"bytes"
	"strings"
)

// officialHeadOrder 各主链路的官方头顺序（原文大小写，抄自真机夹具）。
//
// 末尾的 Host / Connection / Content-Length / Accept-Encoding 是 net/http
// 自己追加的（Host/Connection/Content-Length 由 transport 写，Accept-Encoding
// 由 transport 补 gzip），重排时一并纳入，位置按真机。
var officialHeadOrder = map[string][]string{
	"/v2/chat/completions": {
		"Accept",
		"Content-Type",
		"X-Requested-With",
		"x-stainless-arch",
		"x-stainless-lang",
		"x-stainless-os",
		"x-stainless-package-version",
		"x-stainless-retry-count",
		"x-stainless-runtime",
		"x-stainless-runtime-version",
		"X-Conversation-ID",
		"X-Agent-Purpose",
		"X-IDE-Type",
		"X-IDE-Name",
		"X-IDE-Version",
		"X-Agent-Intent",
		"X-Agent-Runtime",
		"X-Private-Data",
		"acp-connection-id",
		"x-codebuddy-request",
		"X-Request-ID",
		"X-Conversation-Message-ID",
		"X-Conversation-Request-ID",
		"X-Root-Request-ID",
		"X-Agent-Type",
		"baggage",
		"Content-Encoding",
		"traceparent",
		"b3",
		"X-B3-TraceId",
		"X-B3-ParentSpanId",
		"X-B3-SpanId",
		"X-B3-Sampled",
		"X-Trace-ID",
		"X-Product",
		"User-Agent",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Accept-Encoding",
		"Host",
		"Connection",
	},
	"/v2/report": {
		"Accept",
		"Content-Type",
		"X-Request-ID",
		"X-Product",
		"User-Agent",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Accept-Encoding",
		"Host",
		"Connection",
	},
	"/v2/chat/busy": {
		"Accept",
		"Content-Type",
		"User-Agent",
		"X-Request-ID",
		"X-Product",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Accept-Encoding",
		"Host",
		"Connection",
	},
	"/v3/config": {
		// 真机原文里 Connection 紧随 Accept（该链路主动 close），故不放末尾。
		"Accept",
		"Connection",
		"User-Agent",
		"X-Request-ID",
		"X-Product",
		"Content-Length",
		"Accept-Encoding",
		"Host",
	},
	"/v2/plugin/auth/token/refresh": {
		"Accept",
		"Content-Type",
		"X-Domain",
		"X-Refresh-Token",
		"X-Auth-Refresh-Source",
		"User-Agent",
		"X-Request-ID",
		"X-Product",
		"Authorization",
		"X-User-Id",
		"Content-Length",
		"Accept-Encoding",
		"Host",
		"Connection",
	},
}

// headOrderForPath 按请求 path（不含 query）取官方头顺序表。
//
// 表里没有该 path 时返回 nil —— 调用方据此「保持 Go 原生排序」，不做猜测。
func headOrderForPath(path string) []string {
	order, ok := officialHeadOrder[path]
	if !ok {
		return nil
	}
	out := make([]string, len(order))
	copy(out, order)
	return out
}

// headOrderRank 把顺序表压成「小写头名 → 名次」，比对时大小写不敏感
// （Go transport 写出的 key 已是官方拼写，但 Host 之类仍是 canonical）。
func headOrderRank(order []string) map[string]int {
	if len(order) == 0 {
		return nil
	}
	rank := make(map[string]int, len(order))
	for i, k := range order {
		rank[strings.ToLower(k)] = i
	}
	return rank
}

// requestPathOf 从请求行取 path（去掉 query），用于查顺序表。
//
// 请求行形如 "METHOD SP request-target SP HTTP/x.y"。
func requestPathOf(line string) string {
	sp1 := strings.IndexByte(line, ' ')
	if sp1 <= 0 {
		return ""
	}
	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		return ""
	}
	target := rest[:sp2]
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	// absolute-form（http://host/path）时只留 path
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if i := strings.Index(target[8:], "/"); i >= 0 {
			target = target[8+i:]
		}
	}
	return target
}

// isHTTPRequestLine 判断一行是否是可识别的 HTTP/1.x 请求行。
// 认不出来 → 上层原样透传（失败安全）。
func isHTTPRequestLine(line string) bool {
	sp1 := strings.IndexByte(line, ' ')
	if sp1 <= 0 {
		return false
	}
	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 <= 0 {
		return false
	}
	ver := rest[sp2+1:]
	return strings.HasPrefix(ver, "HTTP/1.")
}

// reorderHeadBlockWithOrder 按给定顺序表重排一个完整的请求头块（含结尾空行）。
//
// 行为：
//   - 顺序表里有的头按表序（大小写不敏感匹配）；
//   - 表里没有的头（上游新增、临时注入）保持**原有相对顺序**追加在末尾；
//   - 重复头名（极少）保持各自出现顺序；
//   - 无法识别的请求行 / 空顺序表 → 原样返回，绝不抛错；
//   - 绝不凭空造头：缺 Host / Content-Length 就还是不写（不新增行）。
//
// 纯函数，不依赖任何连接状态，是这套机制唯一需要精确正确的地方。
func reorderHeadBlockWithOrder(block []byte, order []string) []byte {
	if len(block) == 0 || len(order) == 0 {
		return block
	}
	if !bytes.HasSuffix(block, []byte("\r\n\r\n")) {
		return block
	}
	body := block[:len(block)-4] // 去掉终止空行
	lines := strings.Split(string(body), "\r\n")
	if len(lines) < 2 {
		return block
	}
	if !isHTTPRequestLine(lines[0]) {
		return block
	}
	rank := headOrderRank(order)
	if len(rank) == 0 {
		return block
	}

	type entry struct {
		name string // 原始头名（用于大小写不敏感匹配）
		raw  string // 原始行（含可能的续行）
		rank int    // 名次；不在表里 = len(order)
		seq  int    // 原始次序
	}
	var entries []entry
	for i := 1; i < len(lines); i++ {
		ln := lines[i]
		if ln == "" {
			continue
		}
		colon := strings.IndexByte(ln, ':')
		if colon <= 0 {
			// 续行（obsolete line folding）：以空格/制表开头，属于上一个头
			if (ln[0] == ' ' || ln[0] == '\t') && len(entries) > 0 {
				entries[len(entries)-1].raw += "\r\n" + ln
				continue
			}
			// 认不出来的行：当作表外内容，保持原样追加（不丢字节）
			entries = append(entries, entry{name: "", raw: ln, rank: len(order), seq: i})
			continue
		}
		name := ln[:colon]
		r, ok := rank[strings.ToLower(name)]
		if !ok {
			r = len(order)
		}
		entries = append(entries, entry{name: name, raw: ln, rank: r, seq: i})
	}

	// 稳定排序：表内的按名次，表外的排后面并保持原序
	sorted := make([]entry, len(entries))
	copy(sorted, entries)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1].rank > sorted[j].rank; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}

	var out bytes.Buffer
	out.Grow(len(block))
	out.WriteString(lines[0])
	out.WriteString("\r\n")
	for _, e := range sorted {
		out.WriteString(e.raw)
		out.WriteString("\r\n")
	}
	out.WriteString("\r\n")
	return out.Bytes()
}

// reorderHeadBlock 按 path 查表后重排请求头块（便捷包装）。
func reorderHeadBlock(block []byte) []byte {
	return reorderHeadBlockWithOrder(block, headOrderForPath(blockRequestPath(block)))
}

// blockRequestPath 从头块首行取出 path；解析不出返回 ""。
func blockRequestPath(block []byte) string {
	i := bytes.Index(block, []byte("\r\n"))
	if i < 0 {
		return ""
	}
	return requestPathOf(string(block[:i]))
}
