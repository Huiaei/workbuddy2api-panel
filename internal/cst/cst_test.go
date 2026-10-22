package cst

import (
	"testing"
	"time"
)

// TestLocIsUTCPlus8 固定偏移，且不随宿主时区变化。
func TestLocIsUTCPlus8(t *testing.T) {
	if _, off := time.Now().In(Loc).Zone(); off != 8*60*60 {
		t.Fatalf("CST offset=%d want 28800", off)
	}
}

// TestDayCrossesMidnightInCST 上游自然日按 CST 零点切，UTC 主机的「昨天」不算数。
func TestDayCrossesMidnightInCST(t *testing.T) {
	// 2026-10-22T16:30:00Z = CST 2026-10-23 00:30 → 上游已经算「新的一天」。
	utc := time.Date(2026, 10, 22, 16, 30, 0, 0, time.UTC)
	if got := Day(utc); got != "2026-10-23" {
		t.Fatalf("Day=%q want 2026-10-23（UTC 16:30 已是 CST 次日）", got)
	}
	// 2026-10-22T15:30:00Z = CST 23:30 → 仍是当天。
	utc2 := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)
	if got := Day(utc2); got != "2026-10-22" {
		t.Fatalf("Day=%q want 2026-10-22（UTC 15:30 是 CST 当日 23:30）", got)
	}
}

// TestNowCarriesCSTLocation Now 必须带 CST 位置，否则 Hour() 之类仍按宿主算。
func TestNowCarriesCSTLocation(t *testing.T) {
	if name := Now().Location().String(); name != "CST" {
		t.Fatalf("Now().Location()=%q want CST", name)
	}
}
