package upstream

import (
	"testing"
	"time"
)

// TestClassifyProviderOverload11134 11134「模型供应商过载」必须归 ErrUpstreamBusy，
// 不能落到 ErrServer——否则 handler 会喂 Pool.NoteError(uid) 熔断健康账号。
//
// 只认结构化业务码，不认文案：文案是上游 i18n（"the model provider is
// temporarily unavailable"），会变；业务码稳定。
func TestClassifyProviderOverload11134(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{
			name:   "500 + 完整形态",
			status: 500,
			body:   `{"code":11134,"msg":"the model provider is temporarily unavailable, please retry later or switch to another model","requestId":"01a1b4d1b3f45f4b99c406eb04d68909","retry_policy":{"retry":true,"retry_after":8}}`,
			want:   ErrUpstreamBusy,
		},
		{
			name:   "503 + 精简形态",
			status: 503,
			body:   `{"code":11134,"msg":"the model provider is temporarily unavailable"}`,
			want:   ErrUpstreamBusy,
		},
		{
			name:   "500 + 字符串业务码",
			status: 500,
			body:   `{"code":"11134","msg":"provider overloaded"}`,
			want:   ErrUpstreamBusy,
		},
		{
			// 防过宽：只有文案没有业务码 → 仍是 ErrServer（文案不可作为判据）。
			name:   "只有文案没有业务码",
			status: 500,
			body:   `{"msg":"the model provider is temporarily unavailable"}`,
			want:   ErrServer,
		},
		{
			// 防过宽：别的 5xx 业务码不受影响。
			name:   "别的 5xx 业务码",
			status: 500,
			body:   `{"code":11135,"msg":"boom"}`,
			want:   ErrServer,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Errorf("Classify(%d,%s)=%v want %v", c.status, c.body, got, c.want)
			}
		})
	}
}

// TestParseQueueRetryAfter body 侧等待时长解析（11134 的 retry_after）。
//
// 单位按秒推断（上游实际单位未确认），头侧优先、body 侧回落；≤0 判不可用；
// 上限用 retryAfterSanity 封顶。
func TestParseQueueRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
		ok   bool
	}{
		{
			name: "retry_policy 形态",
			body: `{"code":11134,"msg":"provider overloaded","retry_policy":{"retry":true,"retry_after":8}}`,
			want: 8 * time.Second,
			ok:   true,
		},
		{
			name: "data.retry_after 优先",
			body: `{"code":11134,"data":{"retry_after":12},"retry_policy":{"retry_after":8}}`,
			want: 12 * time.Second,
			ok:   true,
		},
		{
			name: "data.retry_after 为 0 时回落 retry_policy",
			body: `{"code":11134,"data":{"retry_after":0},"retry_policy":{"retry_after":8}}`,
			want: 8 * time.Second,
			ok:   true,
		},
		{
			name: "retry_policy 无 retry_after",
			body: `{"code":11134,"msg":"model unavailable","retry_policy":{"retry":true}}`,
			want: 0,
			ok:   false,
		},
		{
			name: "完全没有等待字段",
			body: `{"code":11134,"msg":"model unavailable"}`,
			want: 0,
			ok:   false,
		},
		{
			name: "负值判不可用",
			body: `{"code":11134,"retry_policy":{"retry_after":-5}}`,
			want: 0,
			ok:   false,
		},
		{
			name: "超大值被 retryAfterSanity 封顶",
			body: `{"code":11134,"retry_policy":{"retry_after":100000}}`,
			want: retryAfterSanity,
			ok:   true,
		},
		{
			name: "非 JSON 体",
			body: `not json`,
			want: 0,
			ok:   false,
		},
		{
			name: "空体",
			body: ``,
			want: 0,
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseQueueRetryAfter([]byte(c.body))
			if ok != c.ok {
				t.Errorf("parseQueueRetryAfter ok=%v want %v", ok, c.ok)
			}
			if got != c.want {
				t.Errorf("parseQueueRetryAfter=%v want %v", got, c.want)
			}
		})
	}
}
