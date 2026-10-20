package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 本文件钉住**交叉审计第二轮**抓到的 6 个缺陷的修复行为。
//
// 每一组用例都对应一个**实测复现过的**缺陷（审计报告
// /workspace/xaudit/audit-g1g2.md 的 D2 / D3 / D4 / D5 / D6 / D7 / D8）。
// 判据统一是「**同一错误在两条路径上必须同口径**」与「**请求级问题不得
// 升级成账号级/全局级处置**」——这两条是本次修复的两条主线。

// ---------------------------------------------------------------------------
// D2（BLOCKER）：流式 SSE error 帧分类丢掉 error.code
// ---------------------------------------------------------------------------

// TestFrameKindKeepsBusinessCode 流式 error 帧的分类必须**保留业务码**。
//
// 上游用「HTTP 200 + SSE error 帧」回报错误时，帧形如
// `{"error":{"code":11128,"message":"request illegal"}}`。11-128 与 11140 的
// 判据都在**业务码**上，文案 "request illegal" 是两者共用的。
//
// 旧实现只把 error.message 交给 Classify ⇒ 业务码丢失 ⇒ 11-128 落通用 4xx
// ErrClient ⇒ handler 的 default 分支喂 NoteFailures ⇒ **5 次就把健康号降权
// 出池 10 分钟**。而非流式路径走 Classify(status, 完整 body)，同一个上游错误
// 在那边是 ErrChannelRefused（零动作）——同一个错误两条路径处置相反。
func TestFrameKindKeepsBusinessCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want upstream.ErrKind
	}{
		{"11128 通道校验", `{"error":{"code":11128,"message":"request illegal"}}`, upstream.ErrChannelRefused},
		{"11-128 字符串业务码", `{"error":{"code":"11-128","message":"request illegal"}}`, upstream.ErrChannelRefused},
		{"11140 账号限制", `{"error":{"code":11140,"message":"request illegal"}}`, upstream.ErrAccountFault},
		{"6021 队列满", `{"error":{"code":6021,"message":"queue.full"}}`, upstream.ErrUpstreamBusy},
		// 11140 信封里的模型级限流句在 displayMsg 里——合成信封必须**保留**
		// displayMsg，否则限流否决失效 ⇒ 健康号被永久禁用。
		{"11140 + 模型级限流", `{"error":{"code":11140,"message":"request illegal","displayMsg":{"en":"The model provider is rate-limiting requests."}}}`, upstream.ErrSoftRate},
		// 无业务码：判不准是通道校验还是账号封禁 ⇒ 不罚号（继续轮转）。
		{"无码裸文案", `{"error":{"message":"request illegal"}}`, upstream.ErrChannelRefused},
		// 非 JSON / 无 message：判不出 → ErrNone。
		{"非 JSON", `not json`, upstream.ErrNone},
		{"无 message", `{"error":{"code":11128}}`, upstream.ErrNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstream.FrameKind(tc.body); got != tc.want {
				t.Fatalf("FrameKind(%s)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestFrameKindMatchesNonStreamClassification 流式与非流式对**同一个上游错误**
// 必须给出同一个分类（口径一致是本次修复的核心不变量）。
//
// 注意：帧必须**原样搬运** body 的其余字段（displayMsg 等）——上游把模型级
// 限流说明放在 displayMsg 里，丢掉它就会把「模型侧限流」判成「账号封禁」。
func TestFrameKindMatchesNonStreamClassification(t *testing.T) {
	bodies := []string{
		`{"code":11128,"msg":"request illegal","displayMsg":{"en":"Request blocked. Please send it again"}}`,
		`{"code":11140,"msg":"request illegal"}`,
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"The model provider is rate-limiting requests."}}`,
		`{"code":6021,"msg":"queue.full.title"}`,
	}
	for _, b := range bodies {
		// 非流式：完整 body → Classify。
		want := upstream.Classify(http.StatusBadRequest, b)
		// 流式：error 帧 —— 业务码搬到 error.code、msg 搬到 error.message，
		// 其余字段（displayMsg…）**原样保留**。
		frame := `{"error":{"code":` + businessCodeOf(b) + `,"message":` + jsonQuote(msgOf(b)) + restOfBody(b) + `}}`
		if got := upstream.FrameKind(frame); got != want {
			t.Errorf("流式/非流式分类不一致: body=%s frame=%s stream=%v nonstream=%v", b, frame, got, want)
		}
	}
}

// restOfBody 取出 body 里除 code/msg 之外的字段片段（原样，供拼 error 帧用）。
func restOfBody(body string) string {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return ""
	}
	delete(m, "code")
	delete(m, "msg")
	if len(m) == 0 {
		return ""
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	// json.Marshal 产出的是完整对象（带外层花括号），直接铺进 error 对象里
	// 会得到非法 JSON。这里把外层花括号剥掉，只留 "k":v 片段。
	inner := strings.TrimSpace(string(raw))
	inner = strings.TrimPrefix(inner, "{")
	inner = strings.TrimSuffix(inner, "}")
	return "," + inner
}

// businessCodeOf 从 body 里取出 code 的 JSON 片段（供构造 error 帧用）。
func businessCodeOf(body string) string {
	i := strings.Index(body, `"code":`)
	if i < 0 {
		return "null"
	}
	rest := body[i+len(`"code":`):]
	// 数字或字符串（含连字符形态）。
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		end = len(rest)
	}
	return strings.TrimSpace(rest[:end])
}

// msgOf 从 body 里取出 msg 的值（供构造 error 帧用）。
func msgOf(body string) string {
	i := strings.Index(body, `"msg":`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(`"msg":`):]
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		end = len(rest)
	}
	v := strings.TrimSpace(rest[:end])
	return strings.Trim(v, `"`)
}

// jsonQuote 给字符串加 JSON 引号。
func jsonQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// ---------------------------------------------------------------------------
// D5（MAJOR，影响面最大）：11-128 + 内容审核文案 → 触发全局降级门
// ---------------------------------------------------------------------------

// TestChannelRefusedWithSecurityPolicyInDisplayMsgDoesNotDegrade 11-128 业务码
// 出现在 **displayMsg** 里时，不得被归成内容审核。
//
// 后果链：ErrContentBlocked → handler 执行 h.degrade.Trigger() → **整个网关
// 对所有账号、所有模型**降级到次日 00:00 CST。一次请求级的形状问题被放大成
// 整网关整夜的降级。
//
// 判别依据：只看**主文案字段**（body.msg / error.message）。上游的
// displayMsg/displayTips 是给人看的补充说明，会把审核词塞进去。
func TestChannelRefusedWithSecurityPolicyInDisplayMsgDoesNotDegrade(t *testing.T) {
	body := `{"code":11128,"msg":"request illegal","displayMsg":{"en":"Blocked by security policy"}}`
	if !upstream.IsChannelRefused(body) {
		t.Fatal("11-128 + displayMsg 里的审核词必须仍归通道校验（不得触发全局降级门）")
	}
	if got := upstream.Classify(http.StatusBadRequest, body); got != upstream.ErrChannelRefused {
		t.Fatalf("Classify=%v want %v", got, upstream.ErrChannelRefused)
	}
}

// TestContentBlockedPrimaryMsgStillContentBlocked 上游自带用例的形态
// （主 msg 就是 "blocked by security policy"）仍必须归内容审核：网关的降级
// 重试（换 Degraded 中性提示词）依赖这个分类。
func TestContentBlockedPrimaryMsgStillContentBlocked(t *testing.T) {
	body := `{"code":11128,"msg":"blocked by security policy"}`
	if upstream.IsChannelRefused(body) {
		t.Fatal("主文案声明内容审核时不得归通道校验")
	}
	if got := upstream.Classify(http.StatusBadRequest, body); got != upstream.ErrContentBlocked {
		t.Fatalf("Classify=%v want %v", got, upstream.ErrContentBlocked)
	}
}

// ---------------------------------------------------------------------------
// D6 / D7 / D8（MAJOR）：11140 的判据与惩罚强度
// ---------------------------------------------------------------------------

// Test11140RateLimitVariantsAreNotPermanentBans 11140 信封里的**模型侧限流**
// 措辞变体不得被翻转成永久禁用。
//
// 旧实现只认三条逐字句，于是上游改一个字（"模型提供方限流中" vs
// "模型提供方正在限流"）就把可自愈的限流判成永久封禁。判据必须是**语义
// 家族**。
func Test11140RateLimitVariantsAreNotPermanentBans(t *testing.T) {
	variants := []string{
		`{"code":11140,"msg":"request illegal","displayMsg":{"zh":"模型提供方限流中，请稍后重试"}}`,
		`{"code":11140,"msg":"request illegal","displayMsg":{"zh":"模型服务方限流中"}}`,
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"The model provider is rate limiting requests."}}`,
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"Model provider is rate-limiting requests."}}`,
	}
	for _, b := range variants {
		if upstream.IsAccountRestricted(b) {
			t.Errorf("模型侧限流变体不得判为账号封禁: %s", b)
		}
		if got := upstream.Classify(http.StatusBadRequest, b); got == upstream.ErrAccountFault {
			t.Errorf("模型侧限流变体不得归 ErrAccountFault: %s → %v", b, got)
		}
	}
}

// Test11140BanVariantsStillDetected 真实封禁文案（各种措辞）仍必须被检出 ——
// 判据收窄成「逐字英文句」时，中文/同义文案的封禁会**永不禁用**（账号带病
// 留在池里、面板显示可用、用户无从申诉），那比误封更糟。
func Test11140BanVariantsStillDetected(t *testing.T) {
	variants := []string{
		`{"code":11140,"msg":"request illegal"}`,
		`{"code":11140,"msg":"请求非法"}`,
		`{"code":11140,"msg":"请求非法，账号已被限制"}`,
		`{"code":11140,"msg":"Your account has been restricted."}`,
		`{"code":11140,"msg":"This account was restricted."}`,
		`{"code":11140,"msg":"Account banned."}`,
	}
	for _, b := range variants {
		if !upstream.IsAccountRestricted(b) {
			t.Errorf("真实封禁文案必须被检出: %s", b)
		}
	}
}

// TestAccountRestrictedOn429OnlySoftCooldown **HTTP 429 + 11140** 不得永久禁用
// 账号：429 是瞬时限流语义（几分钟自愈），Classify 自己的注释就写着「宁可短
// 冷却自愈，不可长冷却弃号」。但 11140 的封禁分支排在所有 429 判定之前，
// 于是「429 + 11140」被判成账号封禁 ⇒ 永久 Disable ⇒ 一次限流高峰白扔一个
// 健康号。
//
// 修法在**惩罚侧**分级（分类口径不动，保持与上游既有契约一致）：
// 429 ⇒ 只软冷却；其它状态码 ⇒ 硬禁用。
func TestAccountRestrictedOn429OnlySoftCooldown(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 60 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"code":11140,"msg":"request illegal"}`, "glm-5.2",
		&upstream.Error{Kind: upstream.ErrAccountFault, Status: http.StatusTooManyRequests})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号状态缺失")
	}
	if st.Disabled {
		t.Fatalf("429 + 11140 不得永久禁用账号: %+v", st)
	}
	if !st.Cooling {
		t.Fatalf("429 + 11140 应软冷却（而非永久禁用）: %+v", st)
	}
}

// TestAccountRestrictedOn400StillDisabled 回归守卫：非 429 状态码的 11140
// 仍必须永久禁用（不能因为上面的分级把真封禁也一起放过）。
func TestAccountRestrictedOn400StillDisabled(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 60 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"code":11140,"msg":"request illegal"}`, "glm-5.2",
		&upstream.Error{Kind: upstream.ErrAccountFault, Status: http.StatusBadRequest})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号状态缺失")
	}
	if !st.Disabled {
		t.Fatalf("400 + 11140 必须永久禁用账号: %+v", st)
	}
}
