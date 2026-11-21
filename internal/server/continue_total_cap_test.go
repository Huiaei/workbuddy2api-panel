package server

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// cutSegment 造一段「写满额度被截断」的上游流：正文 + finish=length + usage。
func cutSegment(id string, completion int) string {
	return fmt.Sprintf(`data: {"id":%q,"choices":[{"index":0,"delta":{"content":"段%s"}}]}`+"\n\n", id, id) +
		fmt.Sprintf(`data: {"id":%q,"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":%d,"total_tokens":%d}}`+"\n\n", id, completion, completion+10) +
		"data: [DONE]\n\n"
}

func readReader(t *testing.T, r io.Reader) string {
	t.Helper()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

// TestOutputCapOnlyForLimitedRequests 累计输出上限只管带限额的请求。
//
// 与 reporter 的差别（裁剪项）：reporter 的单段有效额度是 max(客户端限额, 档位下限)，
// 本仓库没有「档位输出预算下限」特性（没有 upstream.MinOutputBudget / OutputBudgetFloor /
// EnsureMinBudget），单段额度就是客户端限额本身；限额不可解析时用兜底值。
func TestOutputCapOnlyForLimitedRequests(t *testing.T) {
	for _, c := range []struct {
		name     string
		limitSet bool
		limit    int64
		want     float64
	}{
		{"不带限额：不按 token 封顶（按段数）", false, 0, 0},
		{"限额 2048：2048×4", true, 2048, 2048 * continueTotalCapRatio},
		{"长文限额 32000：32000×4", true, 32000, 32000 * continueTotalCapRatio},
		{"限额不可解析：兜底", true, 0, defaultContinueTotalCap},
	} {
		r := &continueReader{limitSet: c.limitSet, limit: c.limit}
		if got := r.outputCap(); got != c.want {
			t.Errorf("%s: outputCap=%v want %v", c.name, got, c.want)
		}
	}
}

// TestContinueNoLimitLongAnswerSegments 不带限额的长文（Codex 这类客户端）每段都能写到
// 上游单段上限（实测 32000），按段数封顶：1 原生段 + noLimitContinueSegments-1 段续写。
func TestContinueNoLimitLongAnswerSegments(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, cutSegment(fmt.Sprintf("s%d", call), 32000)
	})
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(cutSegment("s0", 32000))), 0)
	out := readReader(t, r)
	if len(*reqs) != noLimitContinueSegments-1 {
		t.Fatalf("续写请求数=%d want %d", len(*reqs), noLimitContinueSegments-1)
	}
	if n := strings.Count(out, `"finish_reason"`); n != 1 {
		t.Fatalf("终态帧=%d want 1:\n%s", n, out)
	}
}

// TestContinueStopsAtTotalOutputCap 累计输出到顶后不再续：这是开销的唯一硬闸门。
//
// 客户端额度 32000（≥ clientLimitLongAnswer，属于「要长文」这一档）→ 累计上限 128000；
// 每段 32000 ⇒ 4 段到顶（1 原生 + 3 续写）。
func TestContinueStopsAtTotalOutputCap(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, cutSegment(fmt.Sprintf("s%d", call), 32000)
	})
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":32000,"messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(cutSegment("s0", 32000))), 0)
	out := readReader(t, r)
	if len(*reqs) > 3 {
		t.Fatalf("续写请求数=%d 超过累计上限（32000×4/32000=3）", len(*reqs))
	}
	if !r.continueCapped {
		t.Fatalf("到顶应置 continueCapped（日志要能区分「撞自己的上限」与「上游断流」）")
	}
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Fatalf("到顶后应保留截断终态:\n%s", out)
	}
}

// TestContinueLimitedSmallMaxTokensNotContinued 客户端显式给了小额度（max_tokens=2048）：
// 截断是它要的语义，不续写。
//
// 与 reporter 的差别（裁剪项）：reporter 在这条闸门里有两种例外——思考吃光本段落额度、
// 或网关把额度抬到过档位下限；两者都依赖未移植的思考开关探测 / 档位预算特性，故本仓库
// 只有「额度大到 clientLimitLongAnswer 这一档」的例外（见 TestContinueStopsAtTotalOutputCap）。
func TestContinueLimitedSmallMaxTokensNotContinued(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, cutSegment(fmt.Sprintf("s%d", call), 2048)
	})
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(highClientBody), "1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(cutSegment("s0", 2048))), 0)
	readReader(t, r)
	if len(*reqs) != 0 {
		t.Fatalf("客户端带了小额度不应续写，续写请求数=%d", len(*reqs))
	}
}

// TestContinueNoUsageFallsBackToSegmentCeiling 上游不给 usage 时累计上限无法判定，
// 由段数上限兜底（不能无限续）。
func TestContinueNoUsageFallsBackToSegmentCeiling(t *testing.T) {
	noUsage := `data: {"id":"n","choices":[{"index":0,"delta":{"content":"x"}}]}` + "\n\n" +
		`data: {"id":"n","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, noUsage })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(noUsage)), 0)
	out := readReader(t, r)
	if len(*reqs) != noLimitContinueSegments-1 {
		t.Fatalf("续写请求数=%d want %d（无 usage 时按段数上限收口）", len(*reqs), noLimitContinueSegments-1)
	}
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Fatalf("应保留截断终态:\n%s", out)
	}
}
