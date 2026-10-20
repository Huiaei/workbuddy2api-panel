package upstream

import "testing"

// 11140 账号封禁判定的**排除项**边界（对抗性复核发现的过度宽泛问题）。
//
// 11140 的信封里 msg 与 displayMsg 是两个字段：上游可能给出
// msg="request illegal" 而 displayMsg 带**模型级限流**说明——那是模型侧限流，
// 几分钟自愈，永久禁用账号是错的（重登对限流无效）。
//
// 但排除项**不能**用 softRateMarkers 整张宽词表（"rate limit" / "too many" /
// "usage limit" / "限流"）：那些词同样出现在真封禁文案里——
//
//	"Too many login attempts, please try again later."
//	"Your usage limit has been reached."
//	"This account was restricted. Rate limit abuse detected."
//
// 按宽词表否决 → 真封禁账号**永不禁用**：它一直带病服务，面板还显示「可用」，
// 用户也无从申诉。那比误封更糟（误封至少能被解冻）。
func TestIsAccountRestrictedNarrowVeto(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool // true = 账号封禁（应被 Disable）
	}{
		{
			name: "纯封禁",
			body: `{"code":11140,"msg":"request illegal"}`,
			want: true,
		},
		{
			name: "封禁 + 登录尝试过多文案",
			body: `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Too many login attempts, please try again later."}}`,
			want: true,
		},
		{
			name: "封禁 + 用量上限文案",
			body: `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your usage limit has been reached."}}`,
			want: true,
		},
		{
			name: "封禁 + 风控限流滥用文案",
			body: `{"code":11140,"msg":"request illegal","displayMsg":{"en":"This account was restricted. Rate limit abuse detected."}}`,
			want: true,
		},
		{
			name: "封禁 + 账号请求过多文案",
			body: `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Too many requests from this account."}}`,
			want: true,
		},
		{
			// 上游固定的模型级限流句 → 不是账号封禁，保持 ErrSoftRate。
			name: "11140 信封里的模型级限流句",
			body: `{"code":11140,"msg":"request illegal","displayMsg":{"en":"The model provider is rate-limiting requests."}}`,
			want: false,
		},
		{
			name: "11140 无 request illegal 文案",
			body: `{"code":11140,"msg":"The model provider is rate-limiting requests."}`,
			want: false,
		},
		{
			// 通道校验码不是账号封禁。
			name: "11128 通道校验",
			body: `{"code":11128,"msg":"request illegal"}`,
			want: false,
		},
		{
			name: "裸文案无业务码",
			body: `{"msg":"request illegal"}`,
			want: false,
		},
	}
	for _, c := range cases {
		if got := IsAccountRestricted(c.body); got != c.want {
			t.Errorf("%s: IsAccountRestricted(%s)=%v want %v", c.name, c.body, got, c.want)
		}
	}
}

// TestClassify11140BanVariantsStayAccountFault 封禁文案里含限流词时，分类链
// 仍必须落到 ErrAccountFault（会被 Disable），不得滑到 ErrSoftRate。
func TestClassify11140BanVariantsStayAccountFault(t *testing.T) {
	bodies := []string{
		`{"code":11140,"msg":"request illegal"}`,
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"Too many login attempts, please try again later."}}`,
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your usage limit has been reached."}}`,
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"This account was restricted. Rate limit abuse detected."}}`,
	}
	for _, b := range bodies {
		if got := Classify(403, b); got != ErrAccountFault {
			t.Errorf("Classify(403,%s)=%v want %v", b, got, ErrAccountFault)
		}
	}
}
