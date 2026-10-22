package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// The international chat endpoint rejects an incomplete conversation header trio
// with 400/code 11128, including calls made with a zero-value ChatMeta.
func TestChatStreamConversationChannelHeaders(t *testing.T) {
	for _, realm := range []string{"cn", "global"} {
		for _, tc := range []struct {
			name string
			meta ChatMeta
		}{
			{name: "zero metadata"},
			{name: "request ID only", meta: ChatMeta{ConversationRequestID: "existing-turn"}},
			{name: "explicit conversation", meta: ChatMeta{ConversationID: "existing-dialog", ConversationRequestID: "existing-turn"}},
		} {
			t.Run(realm+"/"+tc.name, func(t *testing.T) {
				headers := make(chan http.Header, 2)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					headers <- r.Header.Clone()
					for _, name := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
						if r.Header.Get(name) == "" {
							w.WriteHeader(http.StatusBadRequest)
							io.WriteString(w, `{"code":11128,"msg":"conversation id missing"}`)
							return
						}
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n")
				}))
				defer srv.Close()
				c := New()
				c.ChatBaseCN, c.ChatBaseGlobal = srv.URL, srv.URL
				a, err := auth.Parse([]byte(`{"uid":"test","accessToken":"test-token","realm":"` + realm + `"}`))
				if err != nil {
					t.Fatal(err)
				}
				var previousID string
				for i := 0; i < 2; i++ {
					rc, status, body, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`), "", tc.meta)
					if err != nil || status != http.StatusOK {
						t.Fatalf("chat: status=%d body=%s err=%v", status, body, err)
					}
					stream, err := io.ReadAll(rc)
					rc.Close()
					if err != nil || !strings.Contains(string(stream), "pong") || !strings.Contains(string(stream), "[DONE]") {
						t.Fatalf("incomplete chat stream: %s err=%v", stream, err)
					}
					h := <-headers
					id := h.Get("X-Conversation-ID")
					if tc.meta.ConversationID != "" {
						if id != tc.meta.ConversationID {
							t.Fatalf("explicit conversation ID changed: %q", id)
						}
					} else if !isUUIDv4(id) || id == previousID {
						// 兜底形态是 **UUID v4（36 字符带横线）**，对齐官方
						// X-Conversation-ID 的双版本抓包形态（issue #51）；
						// 32 hex 是 X-Request-ID 的形态，不是这个位置。
						t.Fatalf("fallback ID must be fresh UUID v4: %q (previous %q)", id, previousID)
					}
					if tc.meta.ConversationRequestID != "" && h.Get("X-Conversation-Request-ID") != tc.meta.ConversationRequestID {
						t.Fatal("explicit request ID changed")
					}
					if h.Get("X-Conversation-Message-ID") != h.Get("X-Request-ID") {
						t.Fatal("message and request IDs lost their existing relationship")
					}
					previousID = id
				}
			})
		}
	}
}

// isUUIDv4 判断 s 是否为小写 UUID v4 带横线形态（8-4-4-4-12，版本位 4、variant 位 8/9/a/b）。
// 用于钉住 X-Conversation-ID 的官方形态（issue #51 双版本抓包）。
func isUUIDv4(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	if s[14] != '4' {
		return false
	}
	if c := s[19]; c != '8' && c != '9' && c != 'a' && c != 'b' {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
