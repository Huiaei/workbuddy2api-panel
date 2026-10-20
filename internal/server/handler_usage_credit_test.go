package server

import (
	"math"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestUsageCreditTotalRequiresValidExplicitCreditAndTokens 锁定计费台账的取值口径。
//
// 合并语义（上游 #104 + 分支 issue #179/#181）：
//   - credit 必须是「显式存在、有限、非负」的数——零是合法值（确认免费）。
//   - total_tokens 必须是「有限正整数」才进 token 口径。
//   - 两者**独立判定**：上游给了 credit 却没给 total_tokens 时，整条观测
//     不能被丢掉（分支 issue #179/#181：否则「积分扣除历史」系统性少记），
//     此时 total 记 0、credit 照收，ok=true。
//   - credit 缺失/非法时，无论 total_tokens 合不合法都不记（避免把「没给
//     费用」当成「确认免费」造出免费观测）。
func TestUsageCreditTotalRequiresValidExplicitCreditAndTokens(t *testing.T) {
	tests := []struct {
		name      string
		usage     map[string]any
		want      bool
		wantTotal int
	}{
		{name: "confirmed free", usage: map[string]any{"credit": float64(0), "total_tokens": float64(10)}, want: true, wantTotal: 10},
		{name: "paid", usage: map[string]any{"credit": float64(0.1), "total_tokens": float64(10)}, want: true, wantTotal: 10},
		{name: "missing credit", usage: map[string]any{"total_tokens": float64(10)}},
		{name: "negative credit", usage: map[string]any{"credit": float64(-0.1), "total_tokens": float64(10)}},
		{name: "nan credit", usage: map[string]any{"credit": math.NaN(), "total_tokens": float64(10)}},
		{name: "infinite credit", usage: map[string]any{"credit": math.Inf(1), "total_tokens": float64(10)}},
		{name: "string credit", usage: map[string]any{"credit": "0", "total_tokens": float64(10)}},
		// 以下六例：credit 合法但 total_tokens 缺失/非法 → 保留 credit 观测，
		// token 口径记 0（分支 issue #179/#181），不再整条丢弃。
		{name: "missing total", usage: map[string]any{"credit": float64(0)}, want: true, wantTotal: 0},
		{name: "zero total", usage: map[string]any{"credit": float64(0), "total_tokens": float64(0)}, want: true, wantTotal: 0},
		{name: "negative total", usage: map[string]any{"credit": float64(0), "total_tokens": float64(-1)}, want: true, wantTotal: 0},
		{name: "fractional total", usage: map[string]any{"credit": float64(0), "total_tokens": float64(1.5)}, want: true, wantTotal: 0},
		{name: "infinite total", usage: map[string]any{"credit": float64(0), "total_tokens": math.Inf(1)}, want: true, wantTotal: 0},
		{name: "out of range total", usage: map[string]any{"credit": float64(0), "total_tokens": math.Exp2(63)}, want: true, wantTotal: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			credit, total, ok := usageCreditTotal(map[string]any{"usage": tt.usage})
			if ok != tt.want {
				t.Fatalf("usageCreditTotal ok=%v, want %v (credit=%v total=%d)", ok, tt.want, credit, total)
			}
			if ok && total != tt.wantTotal {
				t.Fatalf("usageCreditTotal total=%d, want %d (credit=%v)", total, tt.wantTotal, credit)
			}
			if ok && (credit < 0 || math.IsNaN(credit) || math.IsInf(credit, 0)) {
				t.Fatalf("accepted invalid credit: credit=%v total=%d", credit, total)
			}
		})
	}
}

func TestUsageCreditTotalMissingCreditDoesNotCreateFreeModelObservation(t *testing.T) {
	p := pool.New("")
	defer p.Close()
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetModelRateOf(func(realm, model string) string {
		if realm == "global" && model == "deepseek-v4.1-flash" {
			return "0.03"
		}
		return ""
	})

	oldGlobal := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(oldGlobal) })
	p.Add(&auth.Auth{UID: "poor", Domain: "www.workbuddy.ai"})
	p.SetCredits("poor", 99, 0)

	credit, total, known := usageCreditTotal(map[string]any{"usage": map[string]any{
		"prompt_tokens": float64(900), "completion_tokens": float64(100), "total_tokens": float64(1000),
	}})
	if known {
		p.NoteModelCost("poor", "deepseek-v4.1-flash", credit, total)
	}
	status, ok := p.Status("poor")
	if !ok || len(status.ModelCosts) != 0 {
		t.Fatalf("missing credit must not create a free observation: status=%+v ok=%v", status, ok)
	}
	if got := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", "global"); got != nil {
		t.Fatalf("catalog-paid model below credit floor must remain blocked, got %v", got.UID)
	}
}

func TestUsageCreditTotalAcceptsAndRecordsConfirmedZero(t *testing.T) {
	p := pool.New("")
	defer p.Close()
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetModelRateOf(func(realm, model string) string { return "0.03" })
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 99, 0)

	credit, total, known := usageCreditTotal(map[string]any{"usage": map[string]any{
		"credit": float64(0), "total_tokens": float64(1000),
	}})
	if !known || credit != 0 || total != 1000 {
		t.Fatalf("explicit zero is a valid free observation, got credit=%v total=%d known=%v", credit, total, known)
	}
	p.NoteModelCost("poor", "free-model", credit, total)
	if got := p.PickExcludingForRealm(nil, "free-model", ""); got == nil || got.UID != "poor" {
		t.Fatalf("confirmed zero-cost model should remain usable below floor, got %v", got)
	}
}

func TestStreamUsageCreditRejectsNegativeAndInvalidTotal(t *testing.T) {
	reader := &chatStatsReader{}
	reader.parseSSELine(`data: {"usage":{"credit":0,"total_tokens":20.0}}`)
	if credit, ok := reader.Credit(); !ok || credit != 0 {
		t.Fatalf("explicit stream credit zero should be retained, got credit=%v ok=%v", credit, ok)
	}
	if total, ok := reader.TotalTokens(); !ok || total != 20 {
		t.Fatalf("positive integer stream total should be retained, got total=%d ok=%v", total, ok)
	}

	reader.parseSSELine(`data: {"usage":{"credit":-0.1,"total_tokens":20}}`)
	if credit, ok := reader.Credit(); ok {
		t.Fatalf("negative stream credit must be unknown, got credit=%v", credit)
	}
	reader.parseSSELine(`data: {"usage":{"credit":0,"total_tokens":1.5}}`)
	if total, ok := reader.TotalTokens(); ok {
		t.Fatalf("fractional stream total must be unknown, got total=%d", total)
	}
}
