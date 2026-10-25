package upstream

import (
	"testing"
	"time"
)

func TestParsePromoNoteOffpeak(t *testing.T) {
	s := ParsePromoNote("周一至周五 09:00–12:00、14:00–18:00 属高峰原价，非高峰期积分5折")
	if !s.Derived || s.Kind != "offpeak" {
		t.Fatalf("want derived offpeak, got %+v", s)
	}
	if len(s.Windows) != 2 || s.Windows[0] != "09:00-12:00" || s.Windows[1] != "14:00-18:00" {
		t.Fatalf("unexpected windows: %v", s.Windows)
	}
	if !s.WeekdaysOnly {
		t.Fatalf("want weekdays-only")
	}
	if s.Factor == nil || *s.Factor != 0.5 {
		t.Fatalf("want factor 0.5, got %v", s.Factor)
	}

	mon10 := time.Date(2026, 10, 19, 10, 0, 0, 0, promoZone)
	if active, known := s.ActiveAt(mon10); !known || active {
		t.Fatalf("Monday 10:00 should be peak (inactive), got active=%v known=%v", active, known)
	}
	mon13 := time.Date(2026, 10, 19, 13, 0, 0, 0, promoZone)
	if active, _ := s.ActiveAt(mon13); !active {
		t.Fatalf("Monday 13:00 should be off-peak (active)")
	}
	mon19 := time.Date(2026, 10, 19, 19, 0, 0, 0, promoZone)
	if active, _ := s.ActiveAt(mon19); !active {
		t.Fatalf("Monday 19:00 should be off-peak (active)")
	}
	sat10 := time.Date(2026, 10, 24, 10, 0, 0, 0, promoZone)
	if active, _ := s.ActiveAt(sat10); !active {
		t.Fatalf("Saturday 10:00 should be off-peak (active)")
	}
}

func TestParsePromoNoteNightly(t *testing.T) {
	s := ParsePromoNote("每晚 23:00—次日 8:00 积分限时立减，错峰用更省")
	if !s.Derived || s.Kind != "discount" {
		t.Fatalf("want derived discount, got %+v", s)
	}
	if len(s.Windows) != 1 || s.Windows[0] != "23:00-08:00" {
		t.Fatalf("unexpected windows: %v", s.Windows)
	}
	if s.Factor != nil {
		t.Fatalf("nightly note has no percentage, factor must stay nil")
	}

	late := time.Date(2026, 10, 19, 23, 30, 0, 0, promoZone)
	if active, _ := s.ActiveAt(late); !active {
		t.Fatalf("23:30 should be active")
	}
	early := time.Date(2026, 10, 20, 7, 30, 0, 0, promoZone)
	if active, _ := s.ActiveAt(early); !active {
		t.Fatalf("07:30 should be active (cross-midnight)")
	}
	noon := time.Date(2026, 10, 19, 12, 0, 0, 0, promoZone)
	if active, _ := s.ActiveAt(noon); active {
		t.Fatalf("12:00 should be inactive")
	}
}

func TestParsePromoNoteDateOnly(t *testing.T) {
	s := ParsePromoNote("7月6日–10月31日，每日赠送免费额度。")
	if s.Derived {
		t.Fatalf("date-only note must not derive a daily window: %+v", s)
	}
}

func TestParsePromoNoteDaily(t *testing.T) {
	s := ParsePromoNote("每日 22:00–次日 08:00 闲时 5 折")
	if !s.Derived || s.Kind != "discount" {
		t.Fatalf("want derived discount, got %+v", s)
	}
	if len(s.Windows) != 1 || s.Windows[0] != "22:00-08:00" {
		t.Fatalf("unexpected windows: %v", s.Windows)
	}
	if s.Factor == nil || *s.Factor != 0.5 {
		t.Fatalf("want factor 0.5")
	}
}

func TestCreditsRateRoundTrip(t *testing.T) {
	if v, ok := parseCreditsRate("x0.17"); !ok || v != 0.17 {
		t.Fatalf("parse x0.17 failed: %v %v", v, ok)
	}
	if got := formatCreditsRate(0.17 * 0.5); got != "x0.085" {
		t.Fatalf("want x0.085, got %s", got)
	}
	if got := formatCreditsRate(0.5); got != "x0.5" {
		t.Fatalf("want x0.5, got %s", got)
	}
}
