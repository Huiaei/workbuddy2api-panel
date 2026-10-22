// conv_fields.go 把会话轮级字段写进出站消息体，对齐官方桌面端的 chat body 形态。
//
// 官方双版本真机抓包（国际版 5.8.4 / 国内版 5.8.3 同构）显示，桌面端把「这一轮
// 属于哪个 request-id」同时写在**头**和**体**里：
//
//	messages[i] = {
//	  "role": "user",
//	  "content": [{"type":"text","text":"..."}],
//	  "agent": "cli",
//	  "conversationRequestId": "<该轮 X-Conversation-Request-ID>",
//	  "startsNewUserRequest": true,
//	}
//
// 而网关此前只写了头、body 里一条都没有——**头体不一致本身就是可判别的形态**
// （issue #51：标识形态也是指纹，且是比文案更难辩解的一类）。
//
// 边界（宁可少写，不编造）：
//   - 只给「当前轮」的最后一条 user 消息写 conversationRequestId；
//   - 历史消息的轮级 ID 客户端没给（OpenAI 协议无此字段），网关无从得知，**不编造**。
//     对不上的 ID 比缺失更容易被关联分析抓出来，所以这里只补能确定的那一条；
//   - assistant 消息的 messageId / usage / model 等由真实调用产出，历史部分同样不补。
package upstream

import (
	"encoding/json"
	"strings"
)

// agentCLI 官方 chat 出站每条消息都带的 agent 段（两版实测均为 `cli`）。
const agentCLI = "cli"

// injectConversationFields 在已改写的出站 body 上补会话轮级字段。
//
// convReqID 为本次出站的 X-Conversation-Request-ID（handler 在轮转循环外生成、
// 循环内复用，故换号/重试/降级都用同一个 ID）。空串时原样返回（不写空值）。
// body 不可解析时原样返回——与 prepareBody 的语义一致：坏 body 不做二次错误化。
func injectConversationFields(body []byte, convReqID string) []byte {
	if len(body) == 0 || convReqID == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	// 找最后一条 user 消息：它就是「当前轮」的起点（对应官方 startsNewUserRequest）。
	idx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); strings.EqualFold(role, "user") {
			idx = i
			break
		}
	}
	if idx < 0 {
		return body
	}
	cur, ok := msgs[idx].(map[string]any)
	if !ok {
		return body
	}
	// 客户端已自带（转发官方 body 的场景）→ 原值优先，绝不覆盖。
	if v, _ := cur["conversationRequestId"].(string); v != "" {
		return body
	}
	cur["conversationRequestId"] = convReqID
	cur["startsNewUserRequest"] = true
	if v, _ := cur["agent"].(string); v == "" {
		cur["agent"] = agentCLI
	}
	msgs[idx] = cur
	obj["messages"] = msgs
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
