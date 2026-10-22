package server

import "testing"

// 覆盖 content 形态的四类：纯数组 / 字符串+数组混合 / 历史消息夹在中间 / 工具轮的 null。
// 每个用例同时断言 imagePartIfPresent（字节闸门 + 解析）与 hasImagePart（解析本体），
// 防止只改其中一个、另一个退化（两者共用同一份解析，坏了必须一起坏）。
func TestHasImagePartMixedContent(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"纯图片数组", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, true},
		{"字符串system+图片数组", `{"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":[{"type":"text","text":"Describe this"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, true},
		{"图片夹在历史消息中间", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/a.png"}]},{"role":"assistant","content":"A picture."},{"role":"user","content":"Tell me more."}]}`, true},
		{"工具轮content为null", `{"messages":[{"role":"assistant","content":null,"tool_calls":[]},{"role":"tool","content":"done"},{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/a.png"}]}]}`, true},
		{"只有文本part不算带图", `{"messages":[{"role":"system","content":"Describe images"},{"role":"user","content":[{"type":"text","text":"image_url"}]}]}`, false},
		{"正文提到image_url但不带图", `{"metadata":{"image_url":"https://example.com/a.png"},"messages":[{"role":"user","content":"hi"}]}`, false},
		{"消息缺content字段", `{"messages":[{"role":"assistant"}]}`, false},
		{"content是对象不是数组", `{"messages":[{"content":{"type":"image_url"}}]}`, false},
		{"JSON本身残缺", `{"messages":[{"content":[{"type":"image_url"}]}]`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imagePartIfPresent([]byte(tt.body)); got != tt.want {
				t.Errorf("imagePartIfPresent() = %v, want %v", got, tt.want)
			}
			if got := hasImagePart([]byte(tt.body)); got != tt.want {
				t.Errorf("hasImagePart() = %v, want %v", got, tt.want)
			}
		})
	}
}
