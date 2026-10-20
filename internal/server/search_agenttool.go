// search_agenttool.go POST /v1/search：官方 agenttool 联网检索的网关入口（issue #195 的 A 形态）。
//
// 与 /v1/search/messages（search_messages.go）的分工：
//   - /v1/search/messages 是 **Anthropic Messages 兼容形态**的搜索壳，专为 DSH 内置
//     搜索 provider（@deepseek-ai/dsh-web-search-deepseek）零改动接入而做：请求是
//     Messages 形态、响应是带 web_search_tool_result 块的 Messages 形态。
//   - /v1/search 是 **官方 agenttool 透传**：请求 {"query","max_results","deep"}，
//     响应原样透出上游 JSON（{query,type,provider,results,total_results,
//     response_time_ms}），不做二次包装——给需要官方字段的调用方用。
//
// 两个端点共存、互不覆盖：路径不同、语义不同（一个产出 Anthropic 块、一个透传官方
// JSON），共用同一套上游检索实现与同一套账号健康语义。
//
// 选号域：请求可带 "model":"cn:x" 只用其 realm 前缀，或直接 "realm":"global"；都没有 → cn。
//
//	POST /v1/search
//	{"query": "玉林 电梯 维保 新规", "max_results": 5}
//	{"query": "...", "deep": true}   ← 综述式（agentic_search），单次 40~66s
//
// 与本仓既有 search.go 轮转内核的关系：search.go 的 runSearch 是「搜索 + 完整账号池
// 语义（在途租约/token 预刷新/错误策略/换号退避）」的内核，供 /v1/search/messages 使用。
// 本端点是**非对话**端点，取号与失败处置与 images 同量级：单次取号、失败换号上限
// searchAttempts、请求级错误（参数/内容/模型）fail-fast 不烧全池。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// searchAttempts 失败换号上限（与 images 同量级：人工/低频触发，3 次足够绕开坏号）。
const searchAttempts = 3

// agenticSearchBudget 综述式检索（deep=true）的整体预算。
//
// 实测单次 40~66s、上游自身可能再慢；给 150s 上限：既容得下正常综述（<70s），
// 又不会在账号已被上游拖死时无限期占住在途租约（租约=账号名额）。
// 普通检索仍走 upstream 侧的 20s 上限（对齐官方 WEB_SEARCH_TIMEOUT_MS）。
const agenticSearchBudget = 150 * time.Second

// searchRequest POST /v1/search 的请求体。
type searchRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
	Model      string `json:"model"` // 可选，只取 realm 前缀
	Realm      string `json:"realm"` // 可选，优先于 model 前缀
	// Deep 可选：走官方综述式检索（agentic_search），回 done 帧原文
	// （synthesis.content 带引用的综述 + results 整页正文）。实测单次 40~66s，
	// 默认 false 保持 snippet 快路径（~1s）。
	Deep bool `json:"deep"`
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req searchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`query is required（例：{"query":"玉林 电梯 维保 新规","max_results":5}）`)
		return
	}
	realm, _ := resolveModel(req.Model)
	if req.Realm != "" {
		realm = req.Realm
	}
	if realm != "global" {
		realm = "cn"
	}

	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < searchAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		// 在途租约：检索是真实上游调用，不占名额就会绕过 MaxInFlight 限流
		// （上游 fork 的 /v1/search 与 searchtool 都漏了这一步）。
		if !h.cfg.Pool.Acquire(a.UID) {
			continue
		}
		// 租约作用域 = 单次尝试：闭包内的 defer 覆盖「传输失败 / 上游 ≥400 /
		// ctx 取消 / 综述超时 / 写响应失败 / panic 展开」全部出口，
		// 保证名额一定归还、不会把账号长期扣住。
		data, err := func() (json.RawMessage, error) {
			defer h.cfg.Pool.Release(a.UID)
			if req.Deep {
				// 综述式单次 40~66s（实测），给它独立的上限而不是无界等待：
				// 租约最多被占这么久，账号不会因为一次综述被长期扣住。
				ctx, cancel := context.WithTimeout(r.Context(), agenticSearchBudget)
				defer cancel()
				return h.cfg.Upstream.AgenticSearch(ctx, a, query)
			}
			return h.cfg.Upstream.SearchRaw(r.Context(), a, query, req.MaxResults)
		}()
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
		lastErr = err
		var ue *upstream.Error
		if errors.As(err, &ue) {
			// 与 chat/images 同一套失败处置（冷却/熔断/模型负缓存/12153 计数）。
			h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, "", ue)
			// 请求级错误：共享清单（参数/内容/模型）之外，search 的未知 400 也不换号——
			// 检索没有「模型权限」维度，400 是请求字段的属性（实测 15003 unsupported
			// provider / 15013 url is required 都走 400），换号必然同样被拒；
			// 而 429/402/401（限流/见底/会话失效）换号可能有效，故只收 400+ErrClient。
			//
			// ErrNone 是 upstream 侧的「判不出类别」标记（检索业务错误没有可判标记，
			// 故不归 ErrClient 免得改写原文）；它同样属于请求级：换号不会让
			// unsupported provider 变得可用，故一并 fail-fast 并原文透传。
			if requestLevelFailFastKind(ue.Kind) || ue.Kind == upstream.ErrNone ||
				(ue.Status == http.StatusBadRequest && ue.Kind == upstream.ErrClient) {
				status := ue.Status
				if status < 400 {
					status = http.StatusBadRequest
				}
				log.Printf("search realm=%s: 请求级错误 kind=%v，不再换号", realm, ue.Kind)
				writeOpenAIError(w, status, "upstream_error", ue.Msg)
				return
			}
		}
		log.Printf("search %s realm=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), realm, attempt+1, err)
	}

	status, code := http.StatusBadGateway, "upstream_error"
	var ue *upstream.Error
	switch {
	case len(tried) == 0:
		status, code = http.StatusServiceUnavailable, "no_account"
		lastErr = fmt.Errorf("没有可用的 %s 账号", realm)
	case errors.As(lastErr, &ue) && ue.Status >= 400:
		status = ue.Status
	}
	log.Printf("search realm=%s: 全部尝试失败（%d 个账号）：%v", realm, len(tried), lastErr)
	writeOpenAIError(w, status, code, lastErr.Error())
}

// requestLevelFailFastKind 报告该类上游错误是否与账号无关（换号必然同样失败）。
// 出图/检索的域级模型路由（14401）、审核与参数错误都是请求本身的属性，换号只会
// 打满全池。chat 侧另有一套（各账号模型权限可能不同）。
func requestLevelFailFastKind(kind upstream.ErrKind) bool {
	switch kind {
	case upstream.ErrBadParams, upstream.ErrPromptTooLong, upstream.ErrImageInvalid,
		upstream.ErrContentBlocked, upstream.ErrModelBlocked:
		return true
	}
	return false
}
