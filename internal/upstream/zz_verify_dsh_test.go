package upstream

// ZZ_VERIFY=1 时跑：**真实生产 body**（DSH 会话实录取出的 12 条消息 + 183 个真实
// 工具 schema，225KB）经网关自身的 prepareBody 管线出站，打真实 CN 上游。
//
// 目的：前面所有探针都是「手写近似 body」。这个用**网关自己的代码**处理
// **真实生产形状**的 body，验证修复在生产路径上真的通过（而不是只在我手写的
// 简化 body 上通过）。
//
// 环境变量：
//   ZZ_VERIFY=1             开启
//   ZZ_BODY=<path>          真实 body JSON（默认 /tmp/dsh_real_body.json）
//   ZZ_AUTH=<path>          CN 账号 auth.json（默认 zzCNAuthFile）
//   ZZ_MODEL=<model>        默认 minimax-m3.1-flash

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func zzVerifyBodyPath() string {
	if p := os.Getenv("ZZ_BODY"); p != "" {
		return p
	}
	return "/tmp/dsh_real_body.json"
}

// TestZZVerifyDSHFix 用真实生产 body 验证：关闭脱敏 ⇒ 上游 400 11128；
// 开启脱敏（修复后）⇒ 上游 200。
func TestZZVerifyDSHFix(t *testing.T) {
	if os.Getenv("ZZ_VERIFY") == "" {
		t.Skip("设 ZZ_VERIFY=1 才真打上游")
	}
	authFile := os.Getenv("ZZ_AUTH")
	if authFile == "" {
		authFile = zzVerifyCNAuthFile
	}
	raw, err := os.ReadFile(zzVerifyBodyPath())
	if err != nil {
		t.Skipf("拿不到真实 body（离线环境）: %v", err)
	}
	t.Logf("真实生产 body: %d bytes", len(raw))

	a := zzLoadAuthFile(t, authFile)
	c := New()

	for _, tc := range []struct {
		label    string
		sanitize bool
		want     int
	}{
		{"sanitize=off（对照，上游应拦）", false, 400},
		{"sanitize=on（修复，应通过）", true, 200},
	} {
		c.SanitizeFingerprints.Store(tc.sanitize)
		prepared := c.prepareBody(raw, a.Realm(), a.UID, "")
		status, respBody := zzSendPrepared(t, c, a, tc.label, prepared)
		t.Logf("[%s] ⇒ HTTP %d  body=%s", tc.label, status, zzSummary(respBody))
		if status != tc.want {
			t.Errorf("%s: HTTP %d, 期望 %d", tc.label, status, tc.want)
		}
	}
}

// zzLoadAuthFile 读取 auth.json。
func zzLoadAuthFile(t *testing.T, path string) *auth.Auth {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("拿不到 auth 文件（离线环境）: %v", err)
	}
	var envelope struct {
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
		} `json:"auth"`
		Account struct {
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			UID          string `json:"uid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("auth 文件解析失败: %v", err)
	}
	return &auth.Auth{
		AccessToken:  envelope.Auth.AccessToken,
		RefreshToken: envelope.Auth.RefreshToken,
		ExpiresAt:    envelope.Auth.ExpiresAt,
		Domain:       envelope.Auth.Domain,
		UID:          envelope.Account.UID,
		Nickname:     envelope.Account.Nickname,
	}
}

// zzSendPrepared 用网关自己的 ChatHeaders + chatBase + chatPaths 出站（body 已准备好）。
func zzSendPrepared(t *testing.T, c *Client, a *auth.Auth, label string, body []byte) (int, string) {
	t.Helper()
	var lastStatus int
	var lastBody string
	for _, path := range c.chatPaths(a) {
		endpoint := c.chatBase(a) + path
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("%s: 构造请求失败: %v", label, err)
		}
		c.ChatHeaders(req, a, "127.0.0.1", ChatMeta{})
		if os.Getenv("ZZ_DUMP") != "" {
			t.Logf("[%s] 出站 body: %s", label, string(body))
		}
		resp, err := c.chatHTTP().Do(req)
		if err != nil {
			t.Logf("%s: 传输错误: %v", label, err)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		lastStatus = resp.StatusCode
		lastBody = string(raw)
		if resp.StatusCode < 400 {
			return resp.StatusCode, lastBody
		}
	}
	return lastStatus, lastBody
}

// zzVerifyCNAuthFile 默认的 CN 账号 auth.json（仅本地取证用；环境变量可覆盖）。
const zzVerifyCNAuthFile = "/mnt/u/wb2api/auths/workbuddy-b4113534-e90b-4dde-814b-0eed734aca63.json"

// zzSummary 截断上游响应体，便于日志查看。
func zzSummary(body string) string {
	if len(body) > 300 {
		body = body[:300]
	}
	return body
}
