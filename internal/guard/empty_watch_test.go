package guard

import "testing"

// 默认名单刻意留空：上游候选模型名是提交者自己环境里的三个，换台机器就是别人
// 的模型。这里钉死「不配置 = 完全不动」——名单外的模型既不熔断也不禁用。
func TestEmptyWatchNeverTouchesAnyModel(t *testing.T) {
	s := New(t.TempDir()+"/g.json", nil)
	if s == nil {
		t.Fatal("New 返回 nil")
	}
	for _, m := range []string{"hy4-preview-f", "step-5-preview", "hy3", "any-model", "cn:hy4-preview-f"} {
		if s.Watched(m) {
			t.Errorf("空名单却监控 %s", m)
		}
		if s.NoteCredit(m, 1.0) {
			t.Errorf("空名单却熔断了 %s", m)
		}
		if s.Disabled(m) {
			t.Errorf("空名单却禁用了 %s", m)
		}
	}
}
