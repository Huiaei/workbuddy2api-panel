package guard

import (
	"path/filepath"
	"testing"
	"os"
	"time"
)

func TestNoteCreditDisablesWatchedModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trial_guard.json")
	s := newTestState(t, "hy4-preview-f")
	s.path = path

	if s.Disabled("cn:hy4-preview-f") {
		t.Fatal("must start enabled")
	}
	if s.NoteCredit("cn:hy4-preview-f", 0) {
		t.Fatal("zero credit must not disable")
	}
	if s.NoteCredit("other-model", 1.2) {
		t.Fatal("unwatched model must not disable")
	}
	if !s.NoteCredit("cn:hy4-preview-f", 0.12) {
		t.Fatal("watched model with credit>0 must disable")
	}
	if !s.Disabled("hy4-preview-f") {
		t.Fatal("normalized lookup must report disabled")
	}
	if s.NoteCredit("cn:hy4-preview-f", 0.12) {
		t.Fatal("second detection must not report a new disable")
	}
}

func TestStatePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trial_guard.json")
	s := New(path, []string{"hy4-preview-f"})
	s.NoteCredit("hy4-preview-f", 0.5)

	reloaded := New(path, []string{"hy4-preview-f"})
	reloaded.loc = time.Local
	if !reloaded.Disabled("hy4-preview-f") {
		t.Fatal("disabled state must survive reload")
	}
	list := reloaded.List()
	if len(list) != 1 || list[0].Model != "hy4-preview-f" {
		t.Fatalf("unexpected list: %+v", list)
	}
}

func TestTimeFuseDisablesAtDeadline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trial_guard.json")
	s := New(path, []string{"step-5-preview"})

	if s.Disabled("step-5-preview") {
		t.Fatal("must start enabled")
	}
	s.SetDeadline("step-5-preview", time.Now().Add(time.Minute))
	if s.Disabled("step-5-preview") {
		t.Fatal("future deadline must not disable")
	}
	s.SetDeadline("step-5-preview", time.Now().Add(-time.Minute))
	if !s.Disabled("cn:step-5-preview") {
		t.Fatal("past deadline must disable")
	}
	list := s.List()
	if len(list) != 1 || list[0].Reason != "promo_expired" {
		t.Fatalf("unexpected list: %+v", list)
	}

	reloaded := New(path, []string{"step-5-preview"})
	if !reloaded.Disabled("step-5-preview") {
		t.Fatal("time-fused state must survive reload")
	}
}

func TestUnwatchedDeadlineIgnored(t *testing.T) {
	s := newTestState(t, "hy4-preview-f")
	s.SetDeadline("step-5-preview", time.Now().Add(-time.Hour))
	if s.Disabled("step-5-preview") {
		t.Fatal("unwatched model must not be time-fused")
	}
}

func TestLegacyArrayFormatLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trial_guard.json")
	legacy := `[{"model":"hy4-preview-f","reason":"trial_credit_observed","credit":0.1,"detected_at":"2026-10-20T07:10:00+08:00"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(path, []string{"hy4-preview-f"})
	if !s.Disabled("hy4-preview-f") {
		t.Fatal("legacy array format must still load")
	}
}

func TestSetDeadlineExtendsRevivesExpiredFuse(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "g.json"), []string{"step-5-preview"})
	s.SetDeadline("step-5-preview", time.Now().Add(-time.Hour))
	if !s.Disabled("step-5-preview") {
		t.Fatal("past deadline must disable")
	}
	revived := s.SetDeadline("step-5-preview", time.Now().Add(24*time.Hour))
	if !revived {
		t.Fatal("extending to a future deadline must report revival")
	}
	if s.Disabled("step-5-preview") {
		t.Fatal("model must be re-enabled after extension")
	}
}

func TestNoteFreeUntilRevivesCreditFuse(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "g.json"), []string{"hy4-preview-f"})
	s.NoteCredit("hy4-preview-f", 0.5)
	if !s.Disabled("hy4-preview-f") {
		t.Fatal("credit must disable")
	}
	if s.NoteFreeUntil("hy4-preview-f", time.Now().Add(-time.Hour)) {
		t.Fatal("past free-until must not revive")
	}
	if !s.NoteFreeUntil("hy4-preview-f", time.Now().Add(7*24*time.Hour)) {
		t.Fatal("future free-until must revive credit fuse")
	}
	if s.Disabled("hy4-preview-f") {
		t.Fatal("model must be re-enabled by new free info")
	}
}

func TestSameFreeClaimDoesNotReviveAfterCredit(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "g.json"), []string{"step-5-preview"})
	claim := time.Now().Add(7 * 24 * time.Hour)
	s.SetDeadline("step-5-preview", claim)
	s.NoteCredit("step-5-preview", 1.0)
	if !s.Disabled("step-5-preview") {
		t.Fatal("credit must disable")
	}
	if s.NoteFreeUntil("step-5-preview", claim) {
		t.Fatal("the same free claim that already failed must not revive")
	}
	if !s.Disabled("step-5-preview") {
		t.Fatal("must stay disabled on the failed claim")
	}
	// 上游给出不同的新截止时间 → 视为新活动，可以复活。
	if !s.NoteFreeUntil("step-5-preview", claim.Add(24*time.Hour)) {
		t.Fatal("a different (new) claim must revive")
	}
	if s.Disabled("step-5-preview") {
		t.Fatal("model must be enabled by the new claim")
	}
}

func newTestState(t *testing.T, watch ...string) *State {
	t.Helper()
	s := New(filepath.Join(t.TempDir(), "g.json"), watch)
	s.loc = time.Local
	return s
}

func at(h, m int) time.Time {
	return time.Date(2026, 10, 20, h, m, 0, 0, time.Local) // 周二
}

func TestWindowBlockedAt(t *testing.T) {
	s := newTestState(t, "hy4-preview-f", "step-5-preview")

	// allow：只在 23:00-08:00 允许（跨午夜）
	s.SetWindow(WindowRule{Model: "hy4-preview-f", Start: "23:00", End: "08:00", Mode: "allow"})
	if !s.windowBlockedAtLocked("hy4-preview-f", at(10, 0)) {
		t.Fatal("10:00 outside allow window must be blocked")
	}
	if s.windowBlockedAtLocked("hy4-preview-f", at(2, 0)) {
		t.Fatal("02:00 inside allow window must be allowed")
	}

	// block：工作日 09:00-18:00 禁止
	s.SetWindow(WindowRule{Model: "step-5-preview", Start: "09:00", End: "18:00", Days: []int{1, 2, 3, 4, 5}, Mode: "block"})
	if !s.windowBlockedAtLocked("step-5-preview", at(10, 0)) {
		t.Fatal("workday 10:00 must be blocked")
	}
	if s.windowBlockedAtLocked("step-5-preview", at(20, 0)) {
		t.Fatal("workday 20:00 must be allowed")
	}
	saturday := time.Date(2026, 10, 24, 10, 0, 0, 0, time.Local)
	if s.windowBlockedAtLocked("step-5-preview", saturday) {
		t.Fatal("saturday must be allowed")
	}

	// Disabled() 走窗口判定，且不落 disabled 表。
	// 用「覆盖全天」的 block 规则，避免依赖运行时的真实时刻。
	s.RemoveWindows("hy4-preview-f")
	s.SetWindow(WindowRule{Model: "hy4-preview-f", Start: "00:00", End: "23:59", Mode: "block"})
	if !s.Disabled("hy4-preview-f") {
		t.Fatal("block window covering now must make Disabled true")
	}
	if len(s.List()) != 0 {
		t.Fatal("window block must not persist a disabled entry")
	}
	e, ok := s.DisabledEntry("hy4-preview-f")
	if !ok || e.Reason != "window_blocked" {
		t.Fatalf("expected synthesized window_blocked entry, got %+v ok=%v", e, ok)
	}
}

func TestPauseMonitoringSuppressesFuse(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "g.json"), []string{"hy4-preview-f"})
	s.NoteCredit("hy4-preview-f", 0.5)
	if !s.Disabled("hy4-preview-f") {
		t.Fatal("credit must disable")
	}
	if !s.Restore("hy4-preview-f", time.Hour) {
		t.Fatal("restore with pause must succeed")
	}
	if s.NoteCredit("hy4-preview-f", 0.5) {
		t.Fatal("paused model must not be re-fused")
	}
	// 暂停过期后恢复保护
	s.mu.Lock()
	s.paused["hy4-preview-f"] = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	if !s.NoteCredit("hy4-preview-f", 0.5) {
		t.Fatal("after pause expiry credit must fuse again")
	}
}

func TestWindowsPersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")
	s := New(path, []string{"hy4-preview-f"})
	if err := s.SetWindow(WindowRule{Model: "hy4-preview-f", Start: "23:00", End: "08:00", Mode: "allow"}); err != nil {
		t.Fatal(err)
	}
	if s.RemoveWindows("other") != 0 {
		t.Fatal("removing unknown model must be a no-op")
	}
	reloaded := New(path, []string{"hy4-preview-f"})
	rules := reloaded.Windows()
	if len(rules) != 1 || rules[0].Mode != "allow" || rules[0].Start != "23:00" {
		t.Fatalf("unexpected rules after reload: %+v", rules)
	}
	if reloaded.RemoveWindows("hy4-preview-f") != 1 {
		t.Fatal("remove must report 1")
	}
	if len(New(path, nil).Windows()) != 0 {
		t.Fatal("removal must persist")
	}
}

func TestSetWindowValidates(t *testing.T) {
	s := newTestState(t)
	for _, bad := range []WindowRule{
		{Model: "", Start: "23:00", End: "08:00", Mode: "allow"},
		{Model: "m", Start: "25:00", End: "08:00", Mode: "allow"},
		{Model: "m", Start: "23:00", End: "8", Mode: "allow"},
		{Model: "m", Start: "23:00", End: "08:00", Mode: "nope"},
		{Model: "m", Start: "23:00", End: "08:00", Mode: "allow", Days: []int{9}},
	} {
		if err := s.SetWindow(bad); err == nil {
			t.Fatalf("expected validation error for %+v", bad)
		}
	}
}
