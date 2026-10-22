// cache_key.go 注入上游 prompt_cache_key 字段（P0 费用优化）。
//
// 逆向实测（buddy-adapter.ts:706-714）：上游服务端支持 prompt_cache_key，
// 同一段 8k token 前缀：
//   - 不带 → prompt_cache_hit_tokens=0, credit≈0.34
//   - 带   → prompt_cache_hit_tokens=7808, credit≈0.02（费用降 ~17×）
//
// 网关在此为每个出站 chat 请求注入一个稳定、按账号隔离的 cache key，
// 让同一客户端对同一账号的连续请求复用上游前缀缓存。
package upstream

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

// InjectPromptCacheKey 在已改写的出站 body 上注入 prompt_cache_key 字段。
//
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. body 已带 conversation_id / conversationId → 用它做会话哈希源
//  3. 都没有 → 用入站 conversationID 参数（来自 X-Conversation-ID 头解析）
//
// 安全约束——按账号隔离：
//   - 生成键是 **UUID v4 形态（36 字符带横线）**，由 sha256(uid|会话) 确定性派生
//   - uid 参与哈希，跨账号绝不碰撞 → 跨账号缓存键绝不碰撞
//   - 跨账号复用同一 cache key 会让上游命中错账号的前缀缓存、泄露对方对话，故 uid 是硬隔离因子
//
// 为什么不再是 `wb2a-<uid8>-<hex>`（issue #51）：旧格式有三个可识别特征——
// ① 项目缩写 `wb2a` 明文在最前面；② 账号 uid 前 8 位明文写进 X-Conversation-ID
// 与 prompt_cache_key；③ 长度固定 48 字符。官方客户端这些位置恒为 UUID v4。
// 三者叠加就是「本仓独有的标识形态」，属于最直白的一类自证指纹。uid 仍参与哈希，
// 隔离性不变，只是不再明文外泄。
//
// 入参 uid 为账号 UID（空则用 "-"，但仍会注入键；调用方应保证传真实 UID）。
// 入参 conversationID 为网关解析出的会话标识（body 里没有 conversation_id 时用它做哈希源）。
// 两源都空时 convHex 为定值（每次新会话不复用前缀，但仍保留账号隔离段）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不二次错误化）。
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	// 优先级 1：客户端已显式带 key → 绝不覆盖。
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		return body
	}
	// 优先级 2：body 里的 conversation_id / conversationId 做会话哈希源。
	conv := conversationID
	bodyHasConv := false
	if v := strField(obj, "conversation_id"); v != "" {
		conv, bodyHasConv = v, true
	} else if v := strField(obj, "conversationId"); v != "" {
		conv, bodyHasConv = v, true
	}
	key := buildCacheKey(uid, conv)
	obj["prompt_cache_key"] = key
	// 部分上游后端（hy4-preview-f / step-5-preview 等）按 **body 的
	// conversation_id** 分前缀缓存槽，并不认 prompt_cache_key：客户端
	// （zcode / Claude Code 系）不发 conversation_id 时，同一账号上所有
	// 会话塌进同一个缓存槽互相驱逐——实测整段未命中（0%）vs 补齐后 ~92%
	// （issue #169 的 A/B 对照实验：只注入 prompt_cache_key 仍然 0%）。
	// 故在算出会话标识后同步补齐 body 级 conversation_id；客户端已自带
	// 时不覆盖（保持原语义）。
	if conv != "" && !bodyHasConv {
		obj["conversation_id"] = conv
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// buildCacheKey 生成 **UUID v4 形态**的稳定 cache key（36 字符带横线）。
//
// 由 sha256(uid|conversation) 确定性派生：同账号同会话恒得同值（缓存槽稳定），
// 不同会话/不同账号各自独立。派生时把第 7 字节置 version=4、第 9 字节置
// RFC 4122 variant，使输出是**合法 UUID v4**——形态与官方客户端在这些位置
// （X-Conversation-ID / prompt_cache_key / body.conversation_id）的值一致。
//
// 注意：本函数是确定性派生，不是随机 UUID；同一 (uid, conversation) 必须恒得同值，
// 否则缓存槽会漂移（issue #169 的命中率修复依赖这一点）。
func buildCacheKey(uid, conversation string) string {
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// strField 从 map 取 string 字段，非 string 或空串返回 ""。
func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
