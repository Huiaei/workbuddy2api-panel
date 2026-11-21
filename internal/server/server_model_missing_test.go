package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 11102「模型 id 上游不存在」是模型级答复：换遍全池都是同一句。此前它落
// ErrModelBlocked ⇒ 整池白打一遍（每个号一次上游调用 + 各写一笔 (账号,模型)
// 负缓存），末端还回「model_unavailable + 换个账号重试」这种反向建议。
//
// 变异验证：把 handler 的 ErrModelMissing 请求级分支撤掉（或把 Classify 的
// IsModelMissing 判定挪到 IsModelBlocked 之后）⇒ 本用例失败（上游被打 2 次、
// 账户模型负缓存多出 2 行、响应变 503 no_healthy_account）。
func TestChatModelMissingFailsFastWithoutRotation(t *testing.T) {
	cap := &captureUpstream{
		status: http.StatusBadRequest,
		body:   `{"code":11102,"msg":"model [glm-5.2] service info not found"}`,
	}
	h, p, st := newCaptureHandler(t, cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s（想要 400：模型级错误不该伪装成 503 账号故障）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("响应里应带 model_unavailable 错误码：%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "service info not found") {
		t.Fatalf("必须透传上游原文：%s", rec.Body)
	}
	_, auths := cap.snapshot()
	if len(auths) != 1 {
		t.Fatalf("上游调用次数=%d want 1（11102 换号无用，不得轮转），auths=%v", len(auths), auths)
	}
	if auths[0] != "Bearer at-u1" {
		t.Fatalf("应只打粘性号 u1，实际 %s", auths[0])
	}
	// 不得写 (账号,模型) 负缓存——那会把整池刷成「该模型不可用」。
	if rows := p.ModelLockView(); len(rows) != 0 {
		t.Fatalf("11102 不该写账号模型负缓存，实际 %d 行：%+v", len(rows), rows)
	}
	s1, _ := p.Status("u1")
	if s1.Cooling || s1.Disabled || s1.ErrTotal != 0 || s1.BreakerFails != 0 {
		t.Errorf("11102 是模型级错误，不得罚账号：%+v", s1)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "u1" {
		t.Fatalf("粘性绑定必须保留（换号解决不了模型 id 问题），实际 %s ok=%v", uid, ok)
	}
	if st.delCnt != 0 {
		t.Errorf("粘性绑定不得被删除，delCnt=%d", st.delCnt)
	}
}

// 11158 是**账号级**权限问题：换到有权限的号真能成功，所以要保留轮转与
// (账号,模型) 负缓存——别把两个码一起当成请求级。
func TestChatModelForbiddenStillRotates(t *testing.T) {
	cap := &captureUpstream{
		status: http.StatusBadRequest,
		body:   `{"code":11158,"msg":"model [glm-5.2] is not allowed for user type"}`,
	}
	h, p, _ := newCaptureHandler(t, cap)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-2"}}`)))

	_, auths := cap.snapshot()
	if len(auths) < 2 {
		t.Fatalf("11158 应继续轮转到下一个号，实际只打了 %d 次：%v", len(auths), auths)
	}
	if rows := p.ModelLockView(); len(rows) == 0 {
		t.Fatal("11158 必须写 (账号,模型) 负缓存，实际 0 行")
	}
	_ = rec
}

// 裸名 + 跨域回退开启时，11102 反而**必须**写 (账号,模型) 负缓存：选号侧的换域判定只认
// 「首选域 ServableForModelRealm == 0」，负缓存正是那条通路；账号本身不罚（只释放在途
// 租约）。本用例与上面的 fail-fast 用例是一对——同一个错误码，开关开/关走两条语义。
//
// 变异验证：删掉 crossRealm 分支里的 BlockModelBackoff ⇒ 本用例失败（0 行负缓存）。
func TestBareModelCrossDomainFallbackMarksModelLock(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-cn" {
			return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash-sg] service info not found"}`, false
		}
		return 200, sseOK, true
	})
	p := newRealmPool(t)
	p.SetCrossDomainFallback(true)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash-sg","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（想要 200：裸名在 cn 11102 后必须回退 global 成功）", rec.Code, rec.Body)
	}
	rows := p.ModelLockView()
	if len(rows) != 1 || rows[0].Model != "deepseek-v4.1-flash-sg" || rows[0].Realm != "cn" || rows[0].Locked != 1 {
		t.Fatalf("跨域回退依赖模型级负缓存，want 1 行(cn 锁 1 号)，实际 %+v", rows)
	}
	if s, _ := p.Status("cn1"); s.Cooling || s.ErrTotal != 0 || s.BreakerFails != 0 {
		t.Errorf("11102 是模型级错误，不得罚账号：%+v", s)
	}
}
