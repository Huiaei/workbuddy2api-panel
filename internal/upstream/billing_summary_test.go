package upstream

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 构造 global 账号（realm 需要全局开关 + BackfillRealm 双开，见 auth.IsGlobal）。
func globalTestAuth(t *testing.T) *auth.Auth {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	a := &auth.Auth{AccessToken: "at", UID: "u-global"}
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatalf("set realm: %v", err)
	}
	return a
}

func msAt(day int, hour, min int) int64 {
	return time.Date(2026, 12, day, hour, min, 0, 0, softRateResetLoc).UnixMilli()
}

// 复现用户实测：global 号 138 分分散在 12-14~12-18 五批到期，旧实现按码塌缩
// 把整码 138 塞给最早一行的日期（11-19），而那行本周期剩余其实是 0。
// 新实现必须按每行自身的 cycleRemain + 真实到期时刻分布，与「检查」同源。
func TestGlobalUserResourceBucketsPerDetailRow(t *testing.T) {
	a := globalTestAuth(t)
	summary := `{"code":0,"data":{"Packages":[
		{"PackageCode":"TCACA_code_007","CycleTotalCapacity":"840","CycleRemainCapacity":"138.31","TotalCount":28},
		{"PackageCode":"TCACA_code_035","CycleTotalCapacity":"100","CycleRemainCapacity":"0","TotalCount":1}
	]}}`
	// paid 空；free 里：一行 11-19 的旧包（cycleRemain=0，capacityRemain=0，是已用完
	// 的 bonus 包），加 5 行未来批次（12-14 起，首行 18.31、其余各 30）。
	freeRows := `{"code":0,"data":{"Accounts":[
		{"PackageCode":"TCACA_code_007","ResourceId":"old-exhausted","DeductionEndTime":` +
		itoa(msAt(14, 3, 15)) + `,"CycleCapacityRemain":0,"CapacityRemain":0,"CycleCapacityRemainPrecise":0,"CapacityRemainPrecise":0},
		{"PackageCode":"TCACA_code_007","ResourceId":"r1","DeductionEndTime":` + itoa(msAt(14, 3, 15)) +
		`,"CycleCapacityRemainPrecise":"18.31","CapacityRemainPrecise":"0"},
		{"PackageCode":"TCACA_code_007","ResourceId":"r2","DeductionEndTime":` + itoa(msAt(15, 3, 3)) +
		`,"CycleCapacityRemainPrecise":"30","CapacityRemainPrecise":"0"},
		{"PackageCode":"TCACA_code_007","ResourceId":"r3","DeductionEndTime":` + itoa(msAt(16, 2, 41)) +
		`,"CycleCapacityRemainPrecise":"30","CapacityRemainPrecise":"0"},
		{"PackageCode":"TCACA_code_007","ResourceId":"r4","DeductionEndTime":` + itoa(msAt(17, 2, 38)) +
		`,"CycleCapacityRemainPrecise":"30","CapacityRemainPrecise":"0"},
		{"PackageCode":"TCACA_code_007","ResourceId":"r5","DeductionEndTime":` + itoa(msAt(18, 3, 31)) +
		`,"CycleCapacityRemainPrecise":"30","CapacityRemainPrecise":"0"}
	]}}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, billingSummaryPath):
			return jsonResp(200, summary), nil
		case strings.HasSuffix(r.URL.Path, billingPaidPath):
			return jsonResp(200, `{"code":0,"data":{"Accounts":[]}}`), nil
		case strings.HasSuffix(r.URL.Path, billingFreePath):
			return jsonResp(200, freeRows), nil
		}
		return jsonResp(404, `{"code":404,"msg":"unexpected path `+r.URL.Path+`"}`), nil
	})
	remain, total, _, earliestAt, earliestRemaining, buckets, err := c.globalUserResource(a, 0)
	if err != nil {
		t.Fatalf("globalUserResource: %v", err)
	}
	if remain != 138 || total != 940 {
		t.Errorf("remain=%d total=%d want 138/940", remain, total)
	}
	if len(buckets) != 5 {
		t.Fatalf("buckets=%d want 5（12-14~12-18 各一批）, got %+v", len(buckets), buckets)
	}
	want := []struct {
		date   string
		remain int64
	}{
		{"2026-12-14", 18}, {"2026-12-15", 30}, {"2026-12-16", 30},
		{"2026-12-17", 30}, {"2026-12-18", 30},
	}
	var sum int64
	for i, b := range buckets {
		if b.Date != want[i].date || b.Remain != want[i].remain {
			t.Errorf("bucket[%d]=%s/%d want %s/%d", i, b.Date, b.Remain, want[i].date, want[i].remain)
		}
		sum += b.Remain
	}
	if sum != remain {
		t.Errorf("bucket sum=%d != summary remain=%d（明细总额必须与权威口径对齐）", sum, remain)
	}
	if earliestAt.Format("2006-01-02") != "2026-12-14" {
		t.Errorf("earliestAt=%v want 2026-12-14", earliestAt)
	}
	if earliestRemaining != 18 {
		t.Errorf("earliestRemaining=%d want 18", earliestRemaining)
	}
}

// 明细拿不到时绝不伪造批次（旧实现会把整码余额塞给"最早到期"一行）。
func TestGlobalUserResourceNoBucketsWhenNoDetails(t *testing.T) {
	a := globalTestAuth(t)
	summary := `{"code":0,"data":{"Packages":[
		{"PackageCode":"TCACA_code_007","CycleTotalCapacity":"840","CycleRemainCapacity":"138.31"}
	]}}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, billingSummaryPath):
			return jsonResp(200, summary), nil
		case strings.HasSuffix(r.URL.Path, billingPaidPath), strings.HasSuffix(r.URL.Path, billingFreePath):
			return jsonResp(200, `{"code":0,"data":{"Accounts":[]}}`), nil
		}
		return jsonResp(404, `{"code":404}`), nil
	})
	remain, _, _, earliestAt, earliestRemaining, buckets, err := c.globalUserResource(a, 0)
	if err != nil {
		t.Fatalf("globalUserResource: %v", err)
	}
	if remain != 138 {
		t.Errorf("remain=%d want 138（明细失败不能丢总额）", remain)
	}
	if len(buckets) != 0 || !earliestAt.IsZero() || earliestRemaining != 0 {
		t.Errorf("明细空却造出批次/到期日: buckets=%+v earliestAt=%v er=%d", buckets, earliestAt, earliestRemaining)
	}
}

// 035 Free Plan 的 CapacityRemain=100 但本周期 CycleCapacityRemain=0、summary 该码
// remain=0：不能凭空造出 100 分、也不能把它算进任何到期批次。
func TestGlobalUserResourceIgnoresExhaustedCycleCapacity(t *testing.T) {
	a := globalTestAuth(t)
	summary := `{"code":0,"data":{"Packages":[
		{"PackageCode":"TCACA_code_035","CycleTotalCapacity":"100","CycleRemainCapacity":"0"}
	]}}`
	free := `{"code":0,"data":{"Accounts":[
		{"PackageCode":"TCACA_code_035","ResourceId":"free-plan","DeductionEndTime":2051173698000,
		 "CapacityRemain":100,"CapacityRemainPrecise":"100","CycleCapacityRemain":0,"CycleCapacityRemainPrecise":"0"}
	]}}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, billingSummaryPath):
			return jsonResp(200, summary), nil
		case strings.HasSuffix(r.URL.Path, billingPaidPath):
			return jsonResp(200, `{"code":0,"data":{"Accounts":[]}}`), nil
		case strings.HasSuffix(r.URL.Path, billingFreePath):
			return jsonResp(200, free), nil
		}
		return jsonResp(404, `{"code":404}`), nil
	})
	remain, _, _, _, _, buckets, err := c.globalUserResource(a, 0)
	if err != nil {
		t.Fatalf("globalUserResource: %v", err)
	}
	if remain != 0 {
		t.Errorf("remain=%d want 0", remain)
	}
	if len(buckets) != 0 {
		t.Errorf("已用完的 035 码造出假批次: %+v", buckets)
	}
}

// itoa 避免测试里为了拼 JSON 引入 strconv 的噪音（值都是非负 int64）。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
