package upstream

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// promo_note.go 从上游的 hover 文案里反推优惠时段与折扣比例。
//
// 背景：WB 上游的 modelPromotions 有两类条目——
//  1. 带 discount.factor 的「机器可读」条目（限时免费等），直接可信；
//  2. 只有 badge + hover 文案的「错峰使用 / 夜间折扣」条目，factor 缺失。
// 第二类如果原样透出，客户端只能看到「有活动」却不知道「此刻是否便宜」，
// 子代理选模型会误判。这里从文案解析出时段与折扣，作为**派生值**透出，
// 并标记 promo_derived=true 让调用方知道置信度低于上游字段。
//
// 设计原则：只解析能确定的结构；解析不出来就返回 Derived=false，调用方
// 保持 badge 语义（不臆造折扣），宁可少报也不多报。
//
// 已覆盖的官方文案形态（2026-10 实测）：
//   - "周一至周五 09:00–12:00、14:00–18:00 属高峰原价，非高峰期积分5折"
//     -> kind=offpeak，peak 窗口两条，工作日限定，factor=0.5
//   - "每晚 23:00—次日 8:00 积分限时立减，错峰用更省"
//     -> kind=discount，窗口 23:00-08:00，factor 缺失
//   - "每日 22:00–次日 08:00 闲时 5 折"
//     -> kind=discount，窗口 22:00-08:00，factor=0.5

var (
	// 时间区间：支持 – — - ~ ～ 至 到 作为分隔，允许「次日」前缀。
	// 破折号放在字符类末尾，避免被当成范围符。
	promoRangeRe = regexp.MustCompile(
		`(\d{1,2})\s*[:：]\s*(\d{2})\s*[–—~～至到-]\s*(?:次日\s*)?(\d{1,2})\s*[:：]\s*(\d{2})`,
	)
	// 折扣比例：5折 / 2.5折 / 9折。
	promoFactorRe = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*折`)
)

// PromoNoteSchedule 是文案解析结果。Kind 为空表示没解析出可用时段。
type PromoNoteSchedule struct {
	Kind         string   // "offpeak"（窗口是高峰，折扣在窗口外）| "discount"（窗口即折扣窗口）
	Windows      []string // 规范化 "HH:MM-HH:MM"
	WeekdaysOnly bool     // offpeak 且文案限定工作日
	Factor       *float64 // 折扣系数（0.5=五折）；nil=文案没写比例
	Derived      bool     // 是否成功解析（false 时其余字段无意义）
}

// ParsePromoNote 从 hover 文案解析时段/折扣。解析不出返回零值。
func ParsePromoNote(note string) PromoNoteSchedule {
	out := PromoNoteSchedule{}
	text := strings.TrimSpace(note)
	if text == "" {
		return out
	}

	for _, m := range promoRangeRe.FindAllStringSubmatch(text, -1) {
		h1, _ := strconv.Atoi(m[1])
		m1, _ := strconv.Atoi(m[2])
		h2, _ := strconv.Atoi(m[3])
		m2, _ := strconv.Atoi(m[4])
		if h1 > 24 || h2 > 24 || m1 > 59 || m2 > 59 {
			continue
		}
		out.Windows = append(out.Windows, fmt.Sprintf("%02d:%02d-%02d:%02d", h1, m1, h2, m2))
	}
	if len(out.Windows) == 0 {
		return out
	}

	if f := promoFactorRe.FindStringSubmatch(text); f != nil {
		if v, err := strconv.ParseFloat(f[1], 64); err == nil && v > 0 && v <= 10 {
			factor := v / 10.0
			out.Factor = &factor
		}
	} else if strings.Contains(text, "免费") {
		zero := 0.0
		out.Factor = &zero
	}

	// 「非高峰」优先于「高峰」：同一句里两者都出现，语义是「高峰原价、其余折扣」。
	if strings.Contains(text, "非高峰") ||
		(strings.Contains(text, "高峰") && strings.Contains(text, "非")) {
		out.Kind = "offpeak"
		out.WeekdaysOnly = strings.Contains(text, "周一至周五") ||
			strings.Contains(text, "工作日")
	} else {
		out.Kind = "discount"
	}
	out.Derived = true
	return out
}

// ActiveAt 判断 now 是否落在优惠区间内。known=false 表示无法判定。
func (s PromoNoteSchedule) ActiveAt(now time.Time) (active bool, known bool) {
	if !s.Derived || len(s.Windows) == 0 {
		return false, false
	}
	inWindow := s.inside(now)
	switch s.Kind {
	case "offpeak":
		if s.WeekdaysOnly {
			switch now.Weekday() {
			case time.Saturday, time.Sunday:
				return true, true // 周末全天非高峰
			}
		}
		return !inWindow, true
	case "discount":
		return inWindow, true
	default:
		return false, false
	}
}

func (s PromoNoteSchedule) inside(now time.Time) bool {
	cur := now.Hour()*60 + now.Minute()
	for _, w := range s.Windows {
		parts := strings.Split(w, "-")
		if len(parts) != 2 {
			continue
		}
		st, ok1 := promoClock(parts[0])
		ed, ok2 := promoClock(parts[1])
		if !ok1 || !ok2 {
			continue
		}
		if st <= ed {
			if cur >= st && cur < ed {
				return true
			}
		} else if cur >= st || cur < ed { // 跨午夜（23:00-08:00）
			return true
		}
	}
	return false
}

// parseCreditsRate 把 "x0.17" 解析成 0.17；格式不符返回 ok=false。
func parseCreditsRate(credits string) (float64, bool) {
	text := strings.TrimSpace(strings.ToLower(credits))
	text = strings.TrimPrefix(text, "x")
	if text == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// formatCreditsRate 反向格式化，保留至多 3 位小数并去掉尾零（0.085 -> "x0.085"）。
func formatCreditsRate(v float64) string {
	text := strconv.FormatFloat(v, 'f', 3, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "" {
		text = "0"
	}
	return "x" + text
}
