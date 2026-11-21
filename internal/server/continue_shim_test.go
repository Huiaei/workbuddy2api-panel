package server

// 本文件是移植 reporter fork 测试时用的适配层与公共夹具：reporter 的一部分夹具原本
// 定义在未移植的测试文件里（continue_budget_raised_test.go / continue_starved_test.go），
// 这里只保留移植后的用例真正用得上的那一小部分。

import (
	"context"
	"io"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newContinueReaderWithFloor 带「档位输出预算下限」的构造器 —— reporter 侧签名。
//
// 本仓库没有输出预算下限特性（没有 upstream.OutputBudgetFloor / EnsureMinBudget /
// MinOutputBudget，出站请求体一个字都不改），所以 floor 在这里没有对应物：参数保留
// 只为让移植来的用例原样使用，行为与 newContinueReader 完全一致。
//
// 这也是本移植**已知的行为差异**：reporter 会把出站 max_tokens 抬到档位下限、
// 并在「预算刚被抬高」时允许额外的续写一次；我们不做这件事（也不该在这里做——
// 那是 upstream 层的特性，不是续写层的）。
func newContinueReaderWithFloor(ctx context.Context, up *upstream.Client, acct *auth.Auth,
	body []byte, clientIP string, meta upstream.ChatMeta, rc io.ReadCloser, _ int64) *continueReader {
	return newContinueReader(ctx, up, acct, body, clientIP, meta, rc)
}

// highClientBody 客户端显式给了不小的输出限额（max_tokens=2048）、且声明了思考档位。
//
// 用途：把「客户端自己限了长度」这条闸门（canContinue 里的 limitSet 分支）打开——
// 这种请求在撞到 limit 时不续写（客户端要的就是这么多）。
const highClientBody = `{"model":"cn:deepseek-v4.1-flash","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
