// Package guard 提供试用/限免模型的自动熔断。
//
// 两种熔断条件：
//  1. 积分熔断：监控名单内的模型一旦出现 usage.credit > 0（免费试用/限免结束，
//     上游开始计费），立即禁用。
//  2. 时间熔断：模型带有 promo_valid_until（限免活动截止时间）时，到点自动禁用，
//     不必等到第一次扣费。
//
// 熔断后：
//   - /v1/models 不再列出该模型；
//   - /v1/chat/completions 直接拒绝该模型（403）。
//
// 状态写在 JSON 文件里（disabled + deadlines），容器重启后仍然生效。
package guard

import (
	"encoding/json"
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/atomicfile"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry 一条熔断记录。
type Entry struct {
	Model      string    `json:"model"`
	Reason     string    `json:"reason"`
	Credit     float64   `json:"credit"`
	DetectedAt time.Time `json:"detected_at"`
	// FreeClaimUntil 记下「扣费发生时上游声称的免费截止时间」。
	// 若之后上游仍下发同一个截止时间，说明这份免费声明已经失败过，
	// 不能再拿它复活（否则会形成 复活→扣费→熔断 的循环）。
	// 只有新的、不同的截止时间才视为新活动。指针：未登记时不出现在 JSON 里。
	FreeClaimUntil *time.Time `json:"free_claim_until,omitempty"`
}

// WindowRule 时段规则（本地时区）。
//
//	Mode=block  窗口内强制熔断该模型
//	Mode=allow  仅窗口内允许，窗口外强制熔断（适合「夜间才免费」的模型）
//
// Start/End 为 "HH:MM"；Start<=End 为当日窗口，Start>End 跨午夜（如 23:00-08:00）。
// Days 为 1=周一 … 7=周日；空 = 每天。
type WindowRule struct {
	Model string `json:"model"`
	Start string `json:"start"`
	End   string `json:"end"`
	Days  []int  `json:"days,omitempty"`
	Mode  string `json:"mode"`
}

// fileState 状态文件结构。兼容旧版「裸数组 = disabled 列表」。
type fileState struct {
	Disabled  []Entry              `json:"disabled"`
	Deadlines map[string]time.Time `json:"deadlines,omitempty"`
	Windows   []WindowRule         `json:"windows,omitempty"`
	Paused    map[string]time.Time `json:"paused,omitempty"`
}

// State 守卫状态（并发安全）。
type State struct {
	mu        sync.RWMutex
	path      string
	loc       *time.Location // 时段规则的时区（默认 Asia/Shanghai，与上游活动口径一致）
	watch     map[string]struct{}
	disabled  map[string]Entry
	deadlines map[string]time.Time
	windows   []WindowRule
	paused    map[string]time.Time
}

// defaultLocation 时段规则默认时区：上游活动时间均为北京时间。
// 容器 TZ 常为 UTC，若直接用 time.Local 会把窗口算偏 8 小时。
func defaultLocation() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.Local
}

// normalize 统一模型名：小写、去 realm 前缀（cn:/global:）。
func normalize(model string) string {
	m := strings.TrimSpace(strings.ToLower(model))
	m = strings.TrimPrefix(m, "cn:")
	m = strings.TrimPrefix(m, "global:")
	return m
}

// New 创建守卫并从 path 恢复历史状态。watch 为空 = 不监控任何模型（等价关闭）。
func New(path string, watch []string) *State {
	s := &State{
		path:      path,
		loc:       defaultLocation(),
		watch:     map[string]struct{}{},
		disabled:  map[string]Entry{},
		deadlines: map[string]time.Time{},
		paused:    map[string]time.Time{},
	}
	for _, w := range watch {
		if n := normalize(w); n != "" {
			s.watch[n] = struct{}{}
		}
	}
	s.load()
	return s
}

func (s *State) load() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var fs fileState
	if json.Unmarshal(raw, &fs) == nil && (fs.Disabled != nil || fs.Deadlines != nil || fs.Windows != nil || fs.Paused != nil) {
		for _, e := range fs.Disabled {
			if n := normalize(e.Model); n != "" {
				e.Model = n
				s.disabled[n] = e
			}
		}
		for m, t := range fs.Deadlines {
			if n := normalize(m); n != "" && !t.IsZero() {
				s.deadlines[n] = t
			}
		}
		s.windows = append(s.windows, fs.Windows...)
		for m, t := range fs.Paused {
			if n := normalize(m); n != "" && !t.IsZero() {
				s.paused[n] = t
			}
		}
		return
	}
	// 旧版格式：裸数组。
	var list []Entry
	if json.Unmarshal(raw, &list) == nil {
		for _, e := range list {
			if n := normalize(e.Model); n != "" {
				e.Model = n
				s.disabled[n] = e
			}
		}
	}
}

func (s *State) persistLocked() {
	if s.path == "" {
		return
	}
	fs := fileState{
		Disabled:  make([]Entry, 0, len(s.disabled)),
		Deadlines: make(map[string]time.Time, len(s.deadlines)),
		Paused:    make(map[string]time.Time, len(s.paused)),
	}
	for _, e := range s.disabled {
		fs.Disabled = append(fs.Disabled, e)
	}
	for m, t := range s.deadlines {
		fs.Deadlines[m] = t
	}
	for m, t := range s.paused {
		if t.After(time.Now()) {
			fs.Paused[m] = t
		}
	}
	fs.Windows = append(fs.Windows, s.windows...)
	raw, err := json.MarshalIndent(fs, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	// 落盘必须走 atomicfile：Windows 上任何其他进程持有句柄时，裸
	// os.Rename 会返 ERROR_ACCESS_DENIED 而失败，状态就再也写不进去。
	_ = atomicfile.Write(s.path, raw, 0o644)
}

// SetDeadline 为监控名单内的模型登记时间熔断点（来自 promo_valid_until）。
// 若新截止时间在未来、且该模型此前因 promo_expired 被熔断，视为上游延长了
// 活动：自动解除熔断并返回 true。相同的截止时间不会重复落盘。
func (s *State) SetDeadline(model string, at time.Time) bool {
	if s == nil || at.IsZero() {
		return false
	}
	n := normalize(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.watch[n]; !ok {
		return false
	}
	revived := false
	if at.After(time.Now()) {
		if e, ok := s.disabled[n]; ok && e.Reason == "promo_expired" {
			delete(s.disabled, n)
			revived = true
		}
	}
	changed := revived
	if prev, ok := s.deadlines[n]; !ok || !prev.Equal(at) {
		s.deadlines[n] = at
		changed = true
	}
	if changed {
		s.persistLocked()
	}
	return revived
}

// NoteFreeUntil 上游明确给出「未来某个时间点前全时段免费」时调用：
// 更新截止时间，并解除该模型的所有熔断（含 trial_credit_observed）——
// 这是新的免费信息，继续锁死反而会让用户用不了免费额度。
// 返回 true 表示本次解除了既有熔断。
func (s *State) NoteFreeUntil(model string, at time.Time) bool {
	if s == nil || at.IsZero() || !at.After(time.Now()) {
		return false
	}
	n := normalize(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.watch[n]; !ok {
		return false
	}
	revived := false
	if e, ok := s.disabled[n]; ok {
		// 同一份免费声明已经失败过（扣费发生时上游就声称免费到这个时间）：
		// 不再复活，避免 复活→扣费→熔断 的循环。只有不同的新截止时间
		// 才视为上游真的换了新活动。
		if e.Reason == "trial_credit_observed" &&
			e.FreeClaimUntil != nil && e.FreeClaimUntil.Equal(at) {
			return false
		}
		delete(s.disabled, n)
		revived = true
	}
	changed := revived
	if prev, ok := s.deadlines[n]; !ok || !prev.Equal(at) {
		s.deadlines[n] = at
		changed = true
	}
	if changed {
		s.persistLocked()
	}
	return revived
}

// Disabled 报告模型当前是否不可用。
// 判定顺序：时段规则（显式，优先）→ 暂停监控（放过自动熔断）→ 已熔断 →
// 时间熔断（惰性触发并持久化）。
func (s *State) Disabled(model string) bool {
	if s == nil {
		return false
	}
	n := normalize(model)
	now := time.Now().In(s.loc)

	s.mu.RLock()
	_, disabled := s.disabled[n]
	deadline, hasDeadline := s.deadlines[n]
	pausedUntil := s.paused[n]
	windowBlocked := s.windowBlockedAtLocked(n, now)
	s.mu.RUnlock()

	// 显式时段规则优先于一切（用户手动要求此时段不用它）。
	if windowBlocked {
		return true
	}
	// 暂停监控：人工放过，不熔断（时段规则仍然生效，见上）。
	if pausedUntil.After(now) {
		return false
	}
	if disabled {
		return true
	}
	if !hasDeadline || now.Before(deadline) {
		return false
	}
	return s.expireLocked(n)
}

// expireLocked 到点熔断（升级写锁后调用）。
func (s *State) expireLocked(model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.disabled[model]; ok {
		return true
	}
	s.disabled[model] = Entry{
		Model:      model,
		Reason:     "promo_expired",
		DetectedAt: time.Now(),
	}
	s.persistLocked()
	return true
}

// windowBlockedAtLocked 判定时段规则是否要求当前禁用该模型（调用方持锁）。
func (s *State) windowBlockedAtLocked(model string, now time.Time) bool {
	minute := now.Hour()*60 + now.Minute()
	wd := int(now.Weekday()) // 0=周日
	for _, r := range s.windows {
		if normalize(r.Model) != model {
			continue
		}
		if len(r.Days) > 0 {
			hit := false
			for _, d := range r.Days {
				if d%7 == wd { // 1=周一…7=周日 → Go Weekday 0=周日
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		st, ok1 := parseHM(r.Start)
		en, ok2 := parseHM(r.End)
		if !ok1 || !ok2 {
			continue
		}
		in := minute >= st && minute < en
		if st > en { // 跨午夜，如 23:00-08:00
			in = minute >= st || minute < en
		}
		switch strings.ToLower(strings.TrimSpace(r.Mode)) {
		case "allow":
			if !in {
				return true
			}
		default: // block
			if in {
				return true
			}
		}
	}
	return false
}

// parseHM 解析 "HH:MM" 为当日分钟数。
func parseHM(v string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// DisabledEntry 返回熔断详情；未熔断时 ok=false。
// 会惰性触发时间熔断（与 Disabled 同语义），供调用方区分原因。
func (s *State) DisabledEntry(model string) (Entry, bool) {
	if s == nil {
		return Entry{}, false
	}
	n := normalize(model)
	s.mu.RLock()
	windowBlocked := s.windowBlockedAtLocked(n, time.Now().In(s.loc))
	e, hasEntry := s.disabled[n]
	s.mu.RUnlock()
	// 时段规则命中优先汇报（即使 disabled 表里还有旧条目）。
	if windowBlocked {
		return Entry{Model: n, Reason: "window_blocked", DetectedAt: time.Now()}, true
	}
	if hasEntry {
		return e, true
	}
	if !s.Disabled(n) {
		return Entry{}, false
	}
	// 时间熔断刚被惰性触发：再取一次。
	s.mu.RLock()
	e, hasEntry = s.disabled[n]
	s.mu.RUnlock()
	if hasEntry {
		return e, true
	}
	return Entry{Model: n, Reason: "promo_expired", DetectedAt: time.Now()}, true
}

// Watched 报告模型是否在监控名单内。
func (s *State) Watched(model string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.watch[normalize(model)]
	return ok
}

// NoteCredit 在观测到积分消耗时调用：命中监控名单则熔断并持久化。
// 返回 true 表示本次触发了新的熔断。
func (s *State) NoteCredit(model string, credit float64) bool {
	if s == nil || credit <= 0 {
		return false
	}
	n := normalize(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.watch[n]; !ok {
		return false
	}
	// 暂停监控期内不熔断（人工放过）。
	if s.paused[n].After(time.Now()) {
		return false
	}
	if _, ok := s.disabled[n]; ok {
		return false
	}
	entry := Entry{
		Model:      n,
		Reason:     "trial_credit_observed",
		Credit:     credit,
		DetectedAt: time.Now(),
	}
	// 记住扣费发生时上游声称的免费截止时间：同一份声明不能再用来复活。
	if dl, ok := s.deadlines[n]; ok {
		claimed := dl
		entry.FreeClaimUntil = &claimed
	}
	s.disabled[n] = entry
	s.persistLocked()
	return true
}

// Restore 手动恢复一个被熔断的模型。
// pauseFor > 0 时同时进入「暂停监控」：期间不因积分/到期自动熔断
// （显式时段规则仍然生效）。
// 保留 deadline：如果该模型的时间熔断点仍已过期，下一次访问会立刻再次熔断；
// 这样「手动恢复」不会绕过到期保护。
func (s *State) Restore(model string, pauseFor time.Duration) bool {
	if s == nil {
		return false
	}
	n := normalize(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.disabled[n]; !ok && pauseFor <= 0 {
		return false
	}
	delete(s.disabled, n)
	if pauseFor > 0 {
		s.paused[n] = time.Now().Add(pauseFor)
	} else {
		delete(s.paused, n)
	}
	s.persistLocked()
	return true
}

// PausedUntil 返回模型的暂停监控截止（零值 = 未暂停/已过期）。
func (s *State) PausedUntil(model string) time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paused[normalize(model)]
}

// SetWindow 新增一条时段规则并持久化。返回错误时规则不生效。
func (s *State) SetWindow(r WindowRule) error {
	r.Model = normalize(r.Model)
	r.Mode = strings.ToLower(strings.TrimSpace(r.Mode))
	if r.Model == "" {
		return fmt.Errorf("model is required")
	}
	if r.Mode != "block" && r.Mode != "allow" {
		return fmt.Errorf("mode must be block or allow")
	}
	if _, ok := parseHM(r.Start); !ok {
		return fmt.Errorf("start must be HH:MM")
	}
	if _, ok := parseHM(r.End); !ok {
		return fmt.Errorf("end must be HH:MM")
	}
	for _, d := range r.Days {
		if d < 1 || d > 7 {
			return fmt.Errorf("days must be 1..7 (Mon..Sun)")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windows = append(s.windows, r)
	s.persistLocked()
	return nil
}

// RemoveWindows 删除某模型的全部时段规则，返回删除条数。
func (s *State) RemoveWindows(model string) int {
	n := normalize(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]WindowRule, 0, len(s.windows))
	removed := 0
	for _, r := range s.windows {
		if normalize(r.Model) == n {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	if removed > 0 {
		s.windows = kept
		s.persistLocked()
	}
	return removed
}

// Windows 返回全部时段规则（供面板展示）。
func (s *State) Windows() []WindowRule {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]WindowRule, len(s.windows))
	copy(out, s.windows)
	return out
}

// WindowBlockedNow 返回当前被时段规则挡住的模型名（供面板展示）。
func (s *State) WindowBlockedNow() []string {
	if s == nil {
		return nil
	}
	now := time.Now().In(s.loc)
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, r := range s.windows {
		n := normalize(r.Model)
		if n == "" || seen[n] {
			continue
		}
		if s.windowBlockedAtLocked(n, now) {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// PausedList 返回仍在暂停监控期内的模型及截止时间。
func (s *State) PausedList() map[string]time.Time {
	if s == nil {
		return nil
	}
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]time.Time)
	for m, t := range s.paused {
		if t.After(now) {
			out[m] = t
		}
	}
	return out
}

// WatchList 返回监控名单（供面板展示）。
func (s *State) WatchList() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.watch))
	for m := range s.watch {
		out = append(out, m)
	}
	return out
}

// List 返回当前熔断条目（供状态展示）。
func (s *State) List() []Entry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.disabled))
	for _, e := range s.disabled {
		out = append(out, e)
	}
	return out
}

// Deadlines 返回已登记的时间熔断点（供状态展示）。
func (s *State) Deadlines() map[string]time.Time {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]time.Time, len(s.deadlines))
	for m, t := range s.deadlines {
		out[m] = t
	}
	return out
}
