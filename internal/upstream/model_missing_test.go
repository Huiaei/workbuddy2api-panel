package upstream

import (
	"net/http"
	"testing"
)

// 11102「model [X] service info not found」是**模型级**答复：模型 id 在上游不存在时
// 换遍全池都是同一句（issue #280）。此前它与 11158 共用 ErrModelBlocked，于是每个号
// 白打一遍、各写一笔 (账号,模型) 负缓存，末端还回「换个账号重试」这种反向建议。
func TestClassifyModelMissing11102(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"400 字符串业务码", http.StatusBadRequest,
			`{"code":"11102","msg":"model [DeepSeek-V4.1-Flash] service info not found"}`, ErrModelMissing},
		{"400 数字业务码", http.StatusBadRequest,
			`{"code":11102,"msg":"model [x] service info not found"}`, ErrModelMissing},
		{"404 信封内业务码", http.StatusNotFound,
			`{"error":{"code":11102,"message":"model [x] service info not found"}}`, ErrModelMissing},
		{"只有固定文案也能认", http.StatusBadRequest,
			`{"msg":"model [x] service info not found"}`, ErrModelMissing},
		{"11158 仍是 ModelBlocked（换号可能成功）", http.StatusBadRequest,
			`{"code":11158,"msg":"model [x] is not allowed for user type"}`, ErrModelBlocked},
		{"500 上的 11102 不算模型级（落 ErrServer）", http.StatusInternalServerError,
			`{"code":11102}`, ErrServer},
		{"429 上的 11102 走限流语义", http.StatusTooManyRequests,
			`{"code":11102}`, ErrSoftRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("Classify(%d, %s) = %v，想要 %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// IsModelMissing 只认 400/404：别的状态码上出现 11102 不能把它降级成请求级
// （那会让本可轮转到可用号的请求直接 400）。
func TestIsModelMissingStatusGate(t *testing.T) {
	body := `{"code":11102,"msg":"model [x] service info not found"}`
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusForbidden} {
		if IsModelMissing(status, body) {
			t.Fatalf("status %d 不该判成模型 id 不存在", status)
		}
	}
	if IsModelMissing(http.StatusBadRequest, "") {
		t.Fatal("空 body 不该判成模型 id 不存在")
	}
}

// 11102 与 11158 的 reason 前缀必须仍然分开：11158 写 (账号,模型) 负缓存，
// 11102 根本不写。
func TestModelMissingDoesNotShareForbiddenReason(t *testing.T) {
	if ModelBlockReasonFor(`{"code":11158,"msg":"is not allowed for user type"}`) != ModelForbiddenReason {
		t.Fatal("11158 应落到 ModelForbiddenReason")
	}
	if got := ErrModelMissing.String(); got != "model_missing" {
		t.Fatalf("ErrModelMissing.String() = %q，想要 model_missing", got)
	}
}
