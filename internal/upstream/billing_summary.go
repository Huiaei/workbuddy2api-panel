// billing_summary.go 国际版（global realm）计费新口径：summary + paid/free packages。
//
// 为什么单独一条链：官方 5.8.4 国际版的额度查询改成了三件套（真机抓包实测，
// 2026-10-20，www.workbuddy.ai）：
//
//	POST /billing/meter/get-user-resource-summary      {}   ← 当前周期聚合（remain/total）
//	POST /billing/meter/get-user-resource-paid-packages {}  ← 付费包明细（带到期时间）
//	POST /billing/meter/get-user-resource-free-packages {}  ← 免费包明细（带到期时间）
//
// 而本仓原先走的是 CN 那套 `/billing/meter/get-user-resource` 带分页 body
// （PageNumber/PageSize/ProductCode/Status/PackageEndTimeRange*）。国际版对该路径
// 返回的形态已与 CN 不同（summary 里 CycleTotalCapacity / CycleRemainCapacity 是
// **字符串小数**，且没有 Accounts 树），继续按 CN 解析会得到恒 0 的余额——积分保底
// 随即把这些号当耗尽号挡出选号。
//
// 分工：summary 给权威的 remain/total（当前周期口径），paid/free 明细只用来补齐
// 「哪天到期、那一批还剩多少」。两者按 PackageCode 关联——summary 的 Packages 也带
// PackageCode，故能精确对齐，不必靠顺序或名字猜。明细拉不到时降级为「只有总额没有
// 到期批次」（面板少一行到期提醒），绝不因为明细失败把总额也丢掉。
package upstream

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	billingSummaryPath = "/billing/meter/get-user-resource-summary"
	billingPaidPath    = "/billing/meter/get-user-resource-paid-packages"
	billingFreePath    = "/billing/meter/get-user-resource-free-packages"
)

// flexFloat 解析「可能是 JSON 数字也可能是字符串」的容量字段。
//
// 上游同一语义字段两套形态并存：summary 里是字符串小数（"335.47000026"），
// 明细里是 JSON 整数 + 独立的 *Precise 字符串。用这个类型兜住，避免为每个字段
// 写两遍解析，也避免字符串形态被 json.Unmarshal 直接判错整段失败。
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "null" || s == `""` {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// 单个脏字段不该让整次余额查询失败：按 0 处理（该项计不进去，其余照算）。
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

// summaryPackage summary.Packages[] 的单条：当前周期的用量聚合。
type summaryPackage struct {
	PackageCode         string    `json:"PackageCode"`
	CycleTotalCapacity  flexFloat `json:"CycleTotalCapacity"`
	CycleRemainCapacity flexFloat `json:"CycleRemainCapacity"`
	CycleUsedCapacity   flexFloat `json:"CycleUsedCapacity"`
	CapacityUnit        string    `json:"CapacityUnit"`
}

// packageDetail paid/free packages 的单条：与 CN 的 Accounts[] 同构，另带 *Precise
// 字符串形态，以及用于判定到期的三个时间字段。
type packageDetail struct {
	PackageCode         string    `json:"PackageCode"`
	PackageName         string    `json:"PackageName"`
	ResourceId          string    `json:"ResourceId"`       // 明细行唯一标识，paid/free 去重
	DeductionEndTime    int64     `json:"DeductionEndTime"` // epoch 毫秒，真实扣费截止
	ExpiredTime         string    `json:"ExpiredTime"`
	CycleEndTime        string    `json:"CycleEndTime"`
	CapacitySize        flexFloat `json:"CapacitySize"`
	CapacityRemain      flexFloat `json:"CapacityRemain"`
	CapacityUsed        flexFloat `json:"CapacityUsed"`
	CycleCapacitySize   flexFloat `json:"CycleCapacitySize"`
	CycleCapacityRemain flexFloat `json:"CycleCapacityRemain"`
	CycleCapacityUsed   flexFloat `json:"CycleCapacityUsed"`
	// *Precise 优先：整数形态会把 0.47 这类小数截断，两者都在时以 Precise 为准。
	CapacitySizePrecise        flexFloat `json:"CapacitySizePrecise"`
	CapacityRemainPrecise      flexFloat `json:"CapacityRemainPrecise"`
	CycleCapacitySizePrecise   flexFloat `json:"CycleCapacitySizePrecise"`
	CycleCapacityRemainPrecise flexFloat `json:"CycleCapacityRemainPrecise"`
}

// summaryReply / packagesReply 是两条端点的信封内层。
type summaryReply struct {
	Packages                []summaryPackage `json:"Packages"`
	SubscriptionPackageName string           `json:"SubscriptionPackageName"`
	IsPaidUser              bool             `json:"IsPaidUser"`
}

type packagesReply struct {
	Accounts   []packageDetail `json:"Accounts"`
	TotalCount int             `json:"TotalCount"`
}

// globalUserResource 国际版额度查询：summary 给总额，paid/free 明细补到期批次。
// 返回值口径与 CN 路径完全一致（remain/total/expiring/earliest/buckets），
// 上游调用方无感知。
func (c *Client) globalUserResource(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, buckets []ExpiryBucket, err error) {
	var raw json.RawMessage
	// summary 是主口径，失败即整次失败（明细单独拿到也没总额可对）。
	raw, err = c.billingJSON(a, "POST", billingSummaryPath, map[string]any{})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, nil, err
	}
	var sum summaryReply
	if uerr := json.Unmarshal(raw, &sum); uerr != nil {
		return 0, 0, 0, time.Time{}, 0, nil, fmt.Errorf("billing summary parse: %w", uerr)
	}
	now := time.Now()
	// summary 的 Packages 是「当前周期的权威用量」：remain/total 以它为准。
	remainByCode := make(map[string]int64, len(sum.Packages))
	for _, p := range sum.Packages {
		r := int64(p.CycleRemainCapacity)
		s := int64(p.CycleTotalCapacity)
		if r < 0 {
			r = 0
		}
		if r > s && s > 0 {
			r = s
		}
		remain += r
		total += s
		if p.PackageCode != "" {
			remainByCode[p.PackageCode] = r
		}
	}

	// 明细只为「到期批次」服务：失败不影响总额（降级为无到期提醒）。
	codes := make([]string, 0, len(sum.Packages))
	for _, p := range sum.Packages {
		if p.PackageCode != "" {
			codes = append(codes, p.PackageCode)
		}
	}
	details := c.globalPackageDetails(a, codes)
	// 到期批次按「明细行自身剩余 + 真实到期时刻」分布，与「检查」逐包口径同源。
	// 旧实现按 PackageCode 塌缩到最早一行、把整码余额塞给那一天：global 号的 138 分
	// 明明分散在 12-14~12-18 五天，快照却显示「明天到期 138（=全部余额）」，而
	// 「检查」走逐包明细显示 12-14 起的真实批次，两条路径分叉。详见 globalExpiryBuckets。
	expiring, earliestAt, buckets = globalExpiryBuckets(remainByCode, details, soon, now)
	if len(buckets) > 0 {
		earliestRemaining = buckets[0].Remain
	}
	return remain, total, expiring, earliestAt, earliestRemaining, buckets, nil
}

// globalExpiryBuckets 从 summary 的按码余额 + 明细行构造按自然日的到期批次。
//
// 根因：global 号的同一 PackageCode（如 007 Bonus Pack）在同一周期内有几十条明细，
// 每条是独立发放批次（ResourceId 不同、DeductionEndTime 逐日递推），各自带
// CycleCapacityRemain。summary 只给整码 remain 总额。旧实现 best[code] 只保留该码
// 最早到期的一行，再把整码 remain 全记到那一天：huiaei 号「138 分分散在 12-14~12-18
// 五天才到期」被显示成「明天 11-19 到期 138 分（=全部余额）」，而「检查」走逐包明细
// 显示 12-14 起的真实批次。用户看到两种刷新方式余额不一致即源于此。
//
// 三条不变量：
//  1. 每行用本周期剩余（cycleRemain，绝不 fallback 到整包 CapacityRemain——
//     035 Free Plan 的 CapacityRemain=100 是周期已用完的整包额度，不是可作废余额）；
//  2. 每码总额以 summary remain 为权威：明细合计显著超出才按比例缩放；summary 该码
//     remain=0 则整码跳过；
//  3. 明细拿不到 future 正余额行时不伪造批次（宁可少显示，不拿总额冒充最早一天）。
func globalExpiryBuckets(
	remainByCode map[string]int64,
	details []packageDetail,
	soon time.Duration,
	now time.Time,
) (expiring int64, earliestAt time.Time, buckets []ExpiryBucket) {
	// paid/free 两份明细可能含同一行，按 ResourceId（缺失时用字段组合）去重。
	seen := make(map[string]bool, len(details))
	// code -> day -> 该日剩余（明细口径）；dayEnd 记该日最早的到期时刻。
	byCodeDay := map[string]map[string]float64{}
	dayEnd := map[string]map[string]time.Time{}
	for _, d := range details {
		key := d.ResourceId
		if key == "" {
			key = fmt.Sprintf("%s|%d|%v|%v", d.PackageCode, d.DeductionEndTime,
				d.CycleCapacityRemainPrecise, d.CapacityRemainPrecise)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		end, ok := packageEffectiveEnd(d.DeductionEndTime, d.ExpiredTime, "", d.CycleEndTime)
		if !ok || !end.After(now) {
			continue
		}
		r := d.cycleRemain()
		if r <= 0 {
			continue
		}
		day := end.In(softRateResetLoc).Format("2006-01-02")
		if byCodeDay[d.PackageCode] == nil {
			byCodeDay[d.PackageCode] = map[string]float64{}
			dayEnd[d.PackageCode] = map[string]time.Time{}
		}
		byCodeDay[d.PackageCode][day] += r
		if cur, has := dayEnd[d.PackageCode][day]; !has || end.Before(cur) {
			dayEnd[d.PackageCode][day] = end
		}
	}
	byDay := map[string]*ExpiryBucket{}
	for code, days := range byCodeDay {
		sr, known := remainByCode[code]
		if !known || sr <= 0 {
			continue
		}
		var detailSum float64
		for _, r := range days {
			detailSum += r
		}
		// 明细合计显著超过 summary（重复行/口径混入）才缩放；1 分容差内直接用明细，
		// 避免 summary 截断小数后逐行缩放取整令合计反而不等于 summary。
		scale := 1.0
		if detailSum > float64(sr)+1 && detailSum > 0 {
			scale = float64(sr) / detailSum
		}
		for day, r := range days {
			v := int64(r * scale)
			if v <= 0 {
				continue
			}
			end := dayEnd[code][day]
			if earliestAt.IsZero() || end.Before(earliestAt) {
				earliestAt = end
			}
			b, has := byDay[day]
			if !has {
				b = &ExpiryBucket{Date: day, ExpireAt: end}
				byDay[day] = b
			} else if end.Before(b.ExpireAt) {
				b.ExpireAt = end
			}
			b.Remain += v
			if soon > 0 && !end.After(now.Add(soon)) {
				expiring += v
			}
		}
	}
	dates := make([]string, 0, len(byDay))
	for d := range byDay {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	buckets = make([]ExpiryBucket, 0, len(dates))
	for _, d := range dates {
		buckets = append(buckets, *byDay[d])
	}
	return
}

// cycleRemain 明细行的本周期剩余：只取 Cycle 口径，不 fallback 到整包 Capacity。
// 035 Free Plan 的 CapacityRemain=100 但 CycleCapacityRemain=0、summary 该码 remain=0
// （本周期已用完），拿整包值当「到期作废」会凭空多出 100 分且落到 2035 年。
func (d packageDetail) cycleRemain() float64 {
	if d.CycleCapacityRemainPrecise > 0 {
		return float64(d.CycleCapacityRemainPrecise)
	}
	return float64(d.CycleCapacityRemain)
}

// globalPackageDetails 拉 paid + free 两份明细并合并。
// 两条都失败 → nil（调用方降级为「无到期批次」）；一条失败只损失那一类的到期信息。
//
// **两条端点的请求体不同形**（2026-10-20 双版本真机抓包实测，CN/GL 完全一致）：
//
//	paid-packages: {PageNumber, PageSize, PackageCodes, Status:[0,3], NeedRenewInfo:true}
//	               —— **没有** SlicePeriod 时间切片
//	free-packages: {PageNumber, PageSize, PackageCodes, Status:[0,3],
//	               SlicePeriodStartTime:<当日> 00:00:00, SlicePeriodEndTime:<当日> 23:59:59}
//	               —— 带时间切片，**没有** NeedRenewInfo
//
// 此前两条共用同一个 body（都带时间切片、都不带 NeedRenewInfo），与官方客户端不符：
// `NeedRenewInfo` 是「是否要返回续订信息」的开关，paid 包靠它拿续订窗口。
// PackageCodes 用 summary 已给的码缩小扫描范围（不按时间盲扫，避免漏包）；
// Status [0,3] = 有效/已过期都要（过期包带权威 DeductionEndTime）。
func (c *Client) globalPackageDetails(a *auth.Auth, codes []string) []packageDetail {
	now := time.Now().In(softRateResetLoc)
	paidBody := map[string]any{
		"PageNumber":    1,
		"PageSize":      200,
		"PackageCodes":  codes,
		"Status":        []int{0, 3},
		"NeedRenewInfo": true,
	}
	freeBody := map[string]any{
		"PageNumber":           1,
		"PageSize":             200,
		"PackageCodes":         codes,
		"Status":               []int{0, 3},
		"SlicePeriodStartTime": now.Format("2006-01-02") + " 00:00:00",
		"SlicePeriodEndTime":   now.Format("2006-01-02") + " 23:59:59",
	}
	var out []packageDetail
	for _, req := range []struct {
		path string
		body map[string]any
	}{
		{billingPaidPath, paidBody},
		{billingFreePath, freeBody},
	} {
		raw, err := c.billingJSON(a, "POST", req.path, req.body)
		if err != nil {
			continue
		}
		var rep packagesReply
		if json.Unmarshal(raw, &rep) != nil {
			continue
		}
		out = append(out, rep.Accounts...)
	}
	return out
}

// balanceRemain 明细自报的剩余额度：Cycle 期套餐优先，回退整包容量；Precise 形态优先。
func (d packageDetail) balanceRemain() float64 {
	prefer := func(precise, plain, preciseCycle, plainCycle flexFloat) float64 {
		if preciseCycle > 0 || plainCycle > 0 {
			if preciseCycle > 0 {
				return float64(preciseCycle)
			}
			return float64(plainCycle)
		}
		if precise > 0 {
			return float64(precise)
		}
		return float64(plain)
	}
	return prefer(d.CapacityRemainPrecise, d.CapacityRemain,
		d.CycleCapacityRemainPrecise, d.CycleCapacityRemain)
}
