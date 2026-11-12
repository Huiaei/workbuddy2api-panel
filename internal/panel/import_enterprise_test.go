package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestImportCockpitEnterpriseSkipsCheckin 企业版账号导入时不得调用签到端点：
// 上游对企业号直接 400 code 10001「企业账号不支持该操作」（实测 2026-10-07）。
// 导入仍应成功落盘，并照常查询额度（企业额度走 get-enterprise-user-usage）。
func TestImportCockpitEnterpriseSkipsCheckin(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-enterprise-user-usage"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"remain":100,"total":200}}`))
		case strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin"):
			_, _ = w.Write([]byte(`{"code":10001,"msg":"企业账号不支持该操作"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[]}}}}`))
		}
	})

	payload := `[{"id":"a1","uid":"ent-uid","access_token":"at-1","enterprise_id":"ent-99"}]`
	rec := postImport(t, p, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["imported"] != float64(1) {
		t.Fatalf("imported=%v want 1 body=%s", resp["imported"], rec.Body.String())
	}
	a := p.cfg.Pool.AuthByUID("ent-uid")
	if a == nil {
		t.Fatal("企业版账号应被导入")
	}
	if !a.IsEnterprise() {
		t.Fatalf("enterprise_id 未落到 auth：EnterpriseID=%q", a.EnterpriseID)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range paths {
		if strings.HasSuffix(path, "/daily-checkin") {
			t.Fatalf("企业版账号不应调用签到端点，实际请求路径=%v", paths)
		}
	}
}

// TestImportCockpitEnterpriseCamelCase cockpit 导出也可能用 camelCase 的
// enterpriseId；两种写法都要识别成企业版。
func TestImportCockpitEnterpriseCamelCase(t *testing.T) {
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[]}}}}`))
	})
	payload := `[{"id":"a1","uid":"ent-camel","access_token":"at-1","enterpriseId":"ent-77"}]`
	rec := postImport(t, p, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	a := p.cfg.Pool.AuthByUID("ent-camel")
	if a == nil {
		t.Fatal("企业版账号应被导入")
	}
	if !a.IsEnterprise() {
		t.Fatalf("camelCase enterpriseId 未被识别：EnterpriseID=%q", a.EnterpriseID)
	}
}

// TestImportCockpitPersonalStillChecksIn 个人账号导入后仍应照常签到（回归护栏）。
func TestImportCockpitPersonalStillChecksIn(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	p := panelWithUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"remain":10,"total":20}}`))
	})
	payload := `[{"id":"a1","uid":"personal-uid","access_token":"at-1"}]`
	rec := postImport(t, p, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, path := range paths {
		if strings.HasSuffix(path, "/daily-checkin") {
			found = true
		}
	}
	if !found {
		t.Fatalf("个人账号导入后应调用签到端点，实际请求路径=%v", paths)
	}
}
