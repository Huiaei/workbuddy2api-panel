package panel

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
)

// TestAddAPIKeyFeedsCreditHistory 静态 API Key 账号也必须出现在「积分历史」里。
//
// 缺陷：添加 API Key 时，为了探测额度构造的 probe 只带 AccessToken、不带 UID：
//
//	probe := &auth.Auth{AccessToken: key}
//
// 而余额观察者（credithist.Ledger.Observe）以 uid 建基线，uid 为空的观测会被
// **直接丢弃**（"纯空白 uid 与空 uid 一样无效"）。于是静态 Key 账号的额度变化
// 永远进不了积分历史——面板「积分历史」对它是一片空白，看起来像"额度从没变过"。
//
// 注意这里探的是**企业口径**端点（静态 Key 无个人账号身份，get-user-resource 对
// 它恒返回空 Accounts），额度数据本身是对的；缺的只是 uid。
func TestAddAPIKeyFeedsCreditHistory(t *testing.T) {
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get-enterprise-user-usage") {
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":812.45,"limitNum":2000}}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	})

	// 挂上积分历史观察者（生产由 cmd/server/main.go 挂：up.SetCreditObserver(creditHist.Observe)）。
	ledger := credithist.New("", 100)
	p.cfg.Upstream.SetCreditObserver(ledger.Observe)

	const key = "ck_9f3a2b1c.deadbeef"
	rec := postJSON(t, p, "/panel/api/accounts/add_apikey", map[string]any{"access_token": key, "nickname": "钥匙"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	// 第一次观测只建基线（不产生流水行），所以基线必须落在**这个账号的 uid** 上。
	// 旧实现（probe 无 uid）会让基线被丢弃 → 之后所有观测都变成"首次观测"，
	// 积分历史永远为空。
	if got := ledger.Read(0); len(got) != 0 {
		t.Fatalf("首次观测只应建基线，不应产生流水行，得到 %d 条", len(got))
	}

	// 额度变化后应产生一条流水，且 uid 是这个账号的 uid。
	ledger.Observe("apikey-9f3a2b1c", 1000)
	rows := ledger.Read(0)
	if len(rows) != 1 {
		t.Fatalf("额度变化应记 1 条流水，得到 %d 条：%+v", len(rows), rows)
	}
	if rows[0].UID != "apikey-9f3a2b1c" {
		t.Errorf("流水 uid=%q want apikey-9f3a2b1c", rows[0].UID)
	}
}

// TestAddAPIKeyProbeCarriesUID 直接钉住根因：探测用的 probe 必须带 uid。
//
// 走不到账本那层也能测：观察者收到的 uid 必须非空。这是 credithist 丢弃
// 空 uid 观测的**唯一入口**——堵住它，空 uid 的观测就再也进不了账本。
func TestAddAPIKeyProbeCarriesUID(t *testing.T) {
	var seenUID string
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get-enterprise-user-usage") {
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":100,"limitNum":2000}}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	})
	p.cfg.Upstream.SetCreditObserver(func(uid string, credits int64) {
		if seenUID == "" {
			seenUID = uid
		}
	})

	const key = "ck_9f3a2b1c.deadbeef"
	rec := postJSON(t, p, "/panel/api/accounts/add_apikey", map[string]any{"access_token": key})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if seenUID != "apikey-9f3a2b1c" {
		t.Errorf("余额观察者收到的 uid=%q want apikey-9f3a2b1c（空 uid 的观测会被积分历史丢弃）", seenUID)
	}
}

// 防回归：观察者挂载后不应影响正常添加流程（探测失败仍 400、不落盘）。
func TestAddAPIKeyObserverDoesNotBreakFlow(t *testing.T) {
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":99999,"msg":"boom"}`))
	})
	p.cfg.Upstream.SetCreditObserver(func(uid string, credits int64) {})
	rec := postJSON(t, p, "/panel/api/accounts/add_apikey", map[string]any{"access_token": "ck_bad.secret"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("上游失败应 400，实际 code=%d body=%s", rec.Code, rec.Body.String())
	}
}
