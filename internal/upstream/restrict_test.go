package upstream

import "testing"

// 11140 限制文书（「账号存在异常行为 / unusual activity」）的判据必须是 **code +
// 文案双条件**：只看 code 会把 modelRateLimitFamily 的限流号永久禁用，只看文案
// 会把「该模型已被限制使用」这类模型级口径误判成账号封禁。
func TestIsRestricted(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "11140 + 限制文书（中文）",
			body: `{"code":11140,"msg":"账号存在异常行为，已被限制使用","requestId":"aaaabbbbccccdddd"}`,
			want: true,
		},
		{
			// 英文形态大小写不定：判据统一按小写比对。
			name: "11140 + unusual activity（英文，混合大小写）",
			body: `{"code":11140,"msg":"Account restricted due to Unusual Activity"}`,
			want: true,
		},
		{
			// 上游偶有把 code 写成字符串的形态，照判。
			name: "字符串业务码 + 限制文书",
			body: `{"code":"11140","msg":"账号存在异常行为"}`,
			want: true,
		},
		{
			// SSE / 网关嵌套信封形态：业务码在 error 对象里。
			name: "嵌套 error 信封",
			body: `{"error":{"code":11140,"message":"账号存在异常行为，已被限制使用"}}`,
			want: true,
		},
		{
			// 泛化的「已被限制」不是账号级判据：这是模型级口径，换号或等模型恢复
			// 即可，禁用账号是净损失。
			name: "11140 + 该模型已被限制使用（模型级，不得判为账号限制）",
			body: `{"code":11140,"msg":"该模型已被限制使用"}`,
			want: false,
		},
		{
			// "request illegal" 家族归 IsAccountRestricted（另一条形态），
			// 本判据只管限制文书。
			name: "11140 + request illegal",
			body: `{"code":11140,"msg":"request illegal"}`,
			want: false,
		},
		{
			// 别的业务码带同样文案不是 11140：文案可被复制，码才是身份。
			name: "11141 + 限制文书",
			body: `{"code":11141,"msg":"账号存在异常行为"}`,
			want: false,
		},
		{
			// 无业务码：不认。宁可回落通用兜底，也不凭裸文案硬禁用。
			name: "裸限制文案（无业务码）",
			body: `{"msg":"账号存在异常行为，已被限制使用"}`,
			want: false,
		},
		{
			name: "空 body",
			body: ``,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRestricted(tc.body); got != tc.want {
				t.Fatalf("IsRestricted(%s)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}

// IsAccountRestriction 是 server 侧连坐计数的判据（两条账号级形态的并集），
// 必须同时排除限流/请求级/模型级形态——否则一个 6004 限流号会被算成「被封」，
// 把整个域暂停。
func TestIsAccountRestriction(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "11140 限制文书",
			body: `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`,
			want: true,
		},
		{
			name: "11140 request illegal",
			body: `{"code":11140,"msg":"request illegal"}`,
			want: true,
		},
		{
			name: "11140 模型级限流",
			body: `{"code":11140,"msg":"The model provider is rate-limiting requests."}`,
			want: false,
		},
		{
			name: "11140 模型级已被限制",
			body: `{"code":11140,"msg":"该模型已被限制使用"}`,
			want: false,
		},
		{
			// 11-128 的 msg 同为 "request illegal"，但那是通道校验失败（账号健康，
			// Classify 已分流到 ErrChannelRefused），绝不能计入连坐。
			name: "11-128 request illegal（通道校验，非账号限制）",
			body: `{"code":"11-128","msg":"request illegal"}`,
			want: false,
		},
		{
			name: "空 body",
			body: ``,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAccountRestriction(tc.body); got != tc.want {
				t.Fatalf("IsAccountRestriction(%s)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}

// 流式/非流式共用的分类：限制文书必须落在 ErrAccountFault（罚号），而不是沿
// 通用 4xx 兜底滑成 ErrClient（只换号不罚号——那正是把整池号送进风控的路径）。
func TestClassifyRestrictionDocument(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ErrKind
	}{
		{"403 + 限制文书", `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, ErrAccountFault},
		{"400 + 限制文书", `{"code":11140,"msg":"Account restricted due to unusual activity"}`, ErrAccountFault},
		// 既有两条账号级形态零回归。
		{"403 + request illegal", `{"code":11140,"msg":"request illegal"}`, ErrAccountFault},
		{"403 + 请求非法", `{"code":11140,"msg":"请求非法，账号已被限制"}`, ErrAccountFault},
		// 模型级口径保持请求级（换号仍是对的）。
		{"403 + 该模型已被限制使用", `{"code":11140,"msg":"该模型已被限制使用"}`, ErrClient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(403, tc.body); got != tc.want {
				t.Fatalf("Classify(403, %s)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}

// SSE 错误帧：先被 FrameKind 重封成顶层信封再分类，限制文书同样必须落到
// ErrAccountFault——否则流式那一跳既不罚号也不计数，连坐闸看不到任何命中。
func TestFrameKindRestrictionDocument(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  ErrKind
	}{
		{
			name:  "error 帧 + 限制文书",
			frame: `{"error":{"code":11140,"message":"账号存在异常行为，已被限制使用"}}`,
			want:  ErrAccountFault,
		},
		{
			name:  "error 帧 + 该模型已被限制使用（模型级，保持请求级）",
			frame: `{"error":{"code":11140,"message":"该模型已被限制使用"}}`,
			want:  ErrClient,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FrameKind(tc.frame); got != tc.want {
				t.Fatalf("FrameKind(%s)=%v want %v", tc.frame, got, tc.want)
			}
		})
	}
}

// has11140 的两层判据：结构化解析优先、字面形态兜底（半截 JSON / 非标准信封
// 只靠结构化解析会漏判，漏判的代价是把已被限制的号当健康号继续轮换）。
func TestHas11140TwoLayers(t *testing.T) {
	yes := []string{
		`{"code":11140}`,
		`{"code":"11140"}`,
		`{"error":{"code":11140}}`,
		`{"data":{"code":11140,"msg":"x"}}`,
		// 非严格 JSON 的字面形态（结构化解析失败，靠 codeMarker 兜住）。
		`{"code":11140,"msg":"账号存在异常行为`,
	}
	for _, body := range yes {
		if !has11140(body) {
			t.Errorf("has11140(%s)=false want true", body)
		}
	}
	no := []string{
		``,
		`{"code":11141}`,
		`{"code":"11-140"}`,
		`{"msg":"账号存在异常行为"}`,
	}
	for _, body := range no {
		if has11140(body) {
			t.Errorf("has11140(%s)=true want false", body)
		}
	}
}
