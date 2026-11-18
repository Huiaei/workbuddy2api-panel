package upstream

import (
	"net/http"
	"strings"
	"testing"
)

func TestClassifyToolSchemaRejected(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"11129 numeric", http.StatusBadRequest, `{"code":11129,"msg":"invalid function call parameters"}`, ErrBadParams},
		{"11129 string code", http.StatusBadRequest, `{"code":"11129","msg":"x"}`, ErrBadParams},
		{"11152 numeric", http.StatusBadRequest, `{"code":11152,"msg":"tool name invalid"}`, ErrBadParams},
		{"11152 duplicate", http.StatusBadRequest, `{"code":11152,"msg":"duplicate tool name"}`, ErrBadParams},
		{"other 400 stays client", http.StatusBadRequest, `{"code":12345,"msg":"whatever"}`, ErrClient},
		{"500 with 11152 stays server", http.StatusInternalServerError, `{"code":11152,"msg":"x"}`, ErrServer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Fatalf("Classify(%d,%s)=%v want %v", c.status, c.body, got, c.want)
			}
		})
	}
}

func TestIsToolSchemaRejectedOnlyByCode(t *testing.T) {
	if !isToolSchemaRejected(`{"code":11129}`) {
		t.Fatal("11129 not hit")
	}
	if !isToolSchemaRejected(`{"code":11152}`) {
		t.Fatal("11152 not hit")
	}
	if isToolSchemaRejected(`{"code":11151}`) {
		t.Fatal("11151 must not hit")
	}
	if isToolSchemaRejected(`{"msg":"mentions 11152 in text"}`) {
		t.Fatal("plain text must not hit")
	}
}

func TestGatewayHintBadParamsTools(t *testing.T) {
	if h := GatewayHint(ErrBadParams, `{"code":11152,"msg":"tool name invalid"}`, HintContext{}); h == "" || !strings.Contains(h, "tool") {
		t.Fatalf("hint for tool schema rejection = %q", h)
	}
	if h := GatewayHint(ErrBadParams, `{"code":11101,"msg":"Unmarshal chat params failed"}`, HintContext{}); h != "" {
		t.Fatalf("hint for non-tool bad params = %q, want empty", h)
	}
}
