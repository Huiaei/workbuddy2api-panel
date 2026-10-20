package upstream

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 个人口径的 data 判空（issue #222 建议 3 的同源现象）。
//
// 上游偶发 `data: {"Response":{"Data":{"Accounts":null}}}` 或 `data: null`。
// json.Unmarshal(null, &resp) 不报错且 resp 全零 → Accounts 为空 → remain=0、
// total=0，表现为「额度 0，但账号健康」——与「接口偶发失败」无法区分。旧口径
// 会把一次空响应变成余额归零：在配了 credit_floor 的部署里，该号所有收费模型
// 全部选不到（静默出局，直到下次成功刷新）。
func TestPersonalResourceEmptyAccountsPayload(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"msg":"OK","data":null}`,
		`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":null}}}}`,
		`{"code":0,"msg":"OK","data":{"Response":{"Data":{}}}}`,
		`{"code":0,"msg":"OK","data":{"Response":null}}`,
		`{"code":0,"msg":"OK","data":{}}`,
	} {
		c := testClient(func(r *http.Request) (*http.Response, error) {
			return jsonResp(200, body), nil
		})
		remain, total, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at", UID: "u1"}, 0)
		if err == nil {
			t.Errorf("body=%s 期望返回 error，实际 remain=%d total=%d（会被误显示成「额度 0」）",
				body, remain, total)
		}
	}
}

// 空数组是**合法态**：「这个号没有资源包」→ 余额 0 是真的，不该报错。
// 这条钉住判空口径必须是**字段存在性**而不是「数组长度为 0」。
func TestPersonalResourceEmptyAccountsArrayIsLegitimate(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
	})
	remain, total, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at", UID: "u1"}, 0)
	if err != nil {
		t.Fatalf("err=%v（空 Accounts 数组是合法态，不该报错）", err)
	}
	if remain != 0 || total != 0 {
		t.Errorf("remain=%d total=%d want 0/0", remain, total)
	}
}
