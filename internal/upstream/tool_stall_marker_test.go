package upstream

import (
	"strings"
	"testing"
)

// TestDetectToolStallMarkupMarkerIsNotStall 上游把工具调用吐成**正文里的原生标记**
// （DSML 块）时，不能判成停摆。
//
// 为什么：这种形态下上游**确实调了工具**，只是把调用写成了文本标记。若判停摆会：
//   - 白白换号重试一次（多花一次上游调用，客户端多等一轮）；
//   - 在没有标记修复层的分支（v1.13.0 基线）上把「工具其实调了」的回合
//     误报成停摆，污染停摆率统计（issue #149 的可观测性失真）。
//
// 两条路径口径必须一致：
//   - 修复层处理过 → 标记被移走、变成结构化 tool_calls → 不是停摆；
//   - 修复层没处理 → 正文里还留着标记 → 也不是停摆。
func TestDetectToolStallMarkupMarkerIsNotStall(t *testing.T) {
	declared := map[string]bool{"get_weather": true}
	// 正文里含标记开始标记（完整块在增量场景可能被切成两片，所以只看开始标记）。
	body := "我这就查天气" + markupCallOpenTag + "invoke name=\"get_weather\"" + "\uFF5C\uFF5C invoke>"
	resp := map[string]any{
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"content": body},
			},
		},
	}
	info := DetectToolStall(resp, declared, false, false)
	if info.Suspect {
		t.Errorf("正文含原生工具调用标记时不应判停摆: %+v", info)
	}
	if !strings.Contains(body, markupCallOpenTag) {
		t.Fatalf("测试数据构造错误")
	}
}

// TestMarkupCallOpenTagMatchesDSMLConstant 停摆检测认的标记必须与修复层认的
// **完全一致**（否则会出现「修复层认得、检测层不认」的漏判）。
//
// 注意：本断言在 main 分支（dsml.go 在）与 v1.13.0 基线分支（dsml.go 不在）
// 都要成立 —— 检测层的常量是自包含的，不依赖修复层是否存在。
func TestMarkupCallOpenTagMatchesDSMLConstant(t *testing.T) {
	if !strings.HasPrefix(markupCallOpenTag, "<\uFF5C\uFF5C") {
		t.Errorf("标记前缀不对: %q", markupCallOpenTag)
	}
	if !strings.HasSuffix(markupCallOpenTag, " calls>") {
		t.Errorf("标记后缀不对: %q", markupCallOpenTag)
	}
	if !strings.Contains(markupCallOpenTag, "DSML") {
		t.Errorf("标记应含 DSML: %q", markupCallOpenTag)
	}
}

// TestDetectToolStallPlainTextStillStall 对照组：普通正文（无标记）仍然判停摆。
// 防止上面的自防御把「真停摆」也一并放过（那就是漏判了）。
func TestDetectToolStallPlainTextStillStall(t *testing.T) {
	declared := map[string]bool{"get_weather": true}
	resp := map[string]any{
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"content": "我这就去查天气"},
			},
		},
	}
	info := DetectToolStall(resp, declared, false, false)
	if !info.Suspect {
		t.Errorf("普通正文 + 零 tool_calls + stop 应判停摆: %+v", info)
	}
}
