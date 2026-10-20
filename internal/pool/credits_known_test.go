package pool

import (
	"os"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 本文件钉住**交叉审计第二轮 Q6.1** 抓到的缺陷：
//
// 「上游没给额度」与「额度为 0」在池里是**同一个值**（credits=0），于是配了
// credit_floor>0 的部署会**静默挡掉所有新账号的收费模型**：
//
//	[q6.1-fresh] 新账号首次解析就失败 → credits=0 total=0
//	            floor=100 + 模型倍率 0.79 时：
//	            Pick(无模型)                        → 有号=true
//	            PickExcludingForModel(glm-5.2, 收费) → 有号=false   ← 静默挡掉
//
// 号在池里、面板显示「余额 0」、但一个请求都接不到，且没有任何日志解释为什么。
// 修法：区分「**额度未知**」（从没读到过上游余额）与「额度为 0」（读到过且
// 真的是 0）；未知时**不**套用 credit_floor。

// TestUnknownCreditsNotBlockedByCreditFloor 额度未知的账号不得被 credit_floor
// 挡在收费模型之外。
func TestUnknownCreditsNotBlockedByCreditFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	// 上游目录倍率兜底：glm-5.2 收费（tier 1，无本地实测时按牌价判收费）。
	p.SetModelRateOf(func(realm, model string) string {
		if model == "glm-5.2" {
			return "0.79"
		}
		return ""
	})
	p.Add(&auth.Auth{UID: "u1"})

	// 从未读到过余额 ⇒ creditsKnown=false。
	if p.PickExcludingForRealm(nil, "glm-5.2", "") == nil {
		t.Fatal("额度未知的账号不得被 credit_floor 静默挡掉（Q6.1）")
	}
}

// TestZeroCreditsAfterReadIsBlockedByCreditFloor 回归守卫：真正**读到过 0**
// 的账号仍必须被 credit_floor 挡住（不能因为上面的修复把真触底也放过）。
func TestZeroCreditsAfterReadIsBlockedByCreditFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetModelRateOf(func(realm, model string) string {
		if model == "glm-5.2" {
			return "0.79"
		}
		return ""
	})
	p.Add(&auth.Auth{UID: "u1"})

	// 读到过余额，且确实是 0 ⇒ 触底。
	p.SetCredits("u1", 0, 2000)
	if p.PickExcludingForRealm(nil, "glm-5.2", "") != nil {
		t.Fatal("读到 0 余额的账号必须被 credit_floor 挡住（真触底）")
	}
}

// TestCreditsKnownSetOnEveryAuthoritativeRead 所有「读到过权威余额」的入口都
// 必须把 creditsKnown 置 true，否则一个成功读到 0 的账号会被当成「未知」
// 而逃过 credit_floor。
func TestCreditsKnownSetOnEveryAuthoritativeRead(t *testing.T) {
	cases := []struct {
		name string
		feed func(p *Pool, uid string)
	}{
		{"SetCredits", func(p *Pool, uid string) { p.SetCredits(uid, 0, 2000) }},
		{"SetCreditsSnapshot", func(p *Pool, uid string) {
			p.SetCreditsSnapshot(uid, CreditsSnapshot{Remain: 0, Total: 2000})
		}},
		{"ReenableIfCredits", func(p *Pool, uid string) { p.ReenableIfCredits(uid, 0, 2000) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withNoPickGap(t)
			p := New("")
			p.SetCostExploreInterval(0)
			p.SetCreditFloor(100)
			p.SetModelRateOf(func(realm, model string) string {
				if model == "glm-5.2" {
					return "0.79"
				}
				return ""
			})
			p.Add(&auth.Auth{UID: "u1"})

			tc.feed(p, "u1")

			st, ok := p.Status("u1")
			if !ok {
				t.Fatal("账号状态缺失")
			}
			if !st.CreditsKnown {
				t.Fatalf("%s 读到权威余额后 creditsKnown 必须为 true", tc.name)
			}
			if p.PickExcludingForRealm(nil, "glm-5.2", "") != nil {
				t.Fatalf("%s 读到 0 余额后必须被 credit_floor 挡住", tc.name)
			}
		})
	}
}

// TestStatusExposesCreditsUnknown 面板必须能区分「未知」与「0」。
func TestStatusExposesCreditsUnknown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号状态缺失")
	}
	if st.CreditsKnown {
		t.Fatal("从未读到余额时 creditsKnown 必须为 false")
	}
	if st.Credits != 0 {
		t.Fatalf("未知时 credits 仍是 0（现状），得到 %d", st.Credits)
	}
}

// TestCreditsKnownSurvivesStateRoundTrip creditsKnown 必须随状态文件持久化，
// 否则重启后「读到过 0」的账号会退回「未知」状态、逃过 credit_floor。
func TestCreditsKnownSurvivesStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"

	p := New(path)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 0, 2000)
	p.Flush()

	p2 := New(path)
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("重启后账号状态缺失")
	}
	if !st.CreditsKnown {
		t.Fatal("creditsKnown 必须随状态文件持久化")
	}
}

// TestLegacyStateFileTreatsZeroAsUnknown 旧状态文件没有 credits_known 字段：
// 正余额 ⇒ 一定读到过（视为已知）；0 ⇒ 无法区分（保守视为未知，方向是
// 「不静默挡号」）。
func TestLegacyStateFileTreatsZeroAsUnknown(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	// 旧格式：只有 credits，无 credits_known。
	legacy := `{"accounts":{"u1":{"credits":0,"credits_total":2000,"uid":"u1"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("写旧状态文件失败: %v", err)
	}

	p := New(path)
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("旧状态文件里的账号应被加载")
	}
	if st.CreditsKnown {
		t.Fatal("旧状态文件 credits=0 应保守视为「额度未知」")
	}
}
