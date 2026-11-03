package main

import "testing"

// TestPanelURL 覆盖托盘菜单「打开面板」用的地址拼装。
//
// 关键点：listen 可能是通配写法（":7863" / "0.0.0.0:7863" / "[::]:7863"），
// 浏览器打不开 0.0.0.0 / [::]，必须换成回环地址。
func TestPanelURL(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		{":7863", "http://127.0.0.1:7863/panel/"},
		{"0.0.0.0:7863", "http://127.0.0.1:7863/panel/"},
		{"[::]:7863", "http://127.0.0.1:7863/panel/"},
		{"127.0.0.1:7863", "http://127.0.0.1:7863/panel/"},
		{"localhost:9000", "http://localhost:9000/panel/"},
		{"192.168.1.10:7863", "http://192.168.1.10:7863/panel/"},
		{"", "http://127.0.0.1:7863/panel/"},
		{"garbage", "http://127.0.0.1:7863/panel/"},
	}
	for _, tc := range cases {
		if got := panelURL(tc.listen); got != tc.want {
			t.Errorf("panelURL(%q) = %q, want %q", tc.listen, got, tc.want)
		}
	}
}

// TestPanelURLWithKey 覆盖托盘菜单「打开面板」时把密钥放进片段的行为。
//
// 关键点：
//   - 片段（# 之后）不进 HTTP 请求，所以服务器日志里不会留密钥；前端拿到后立刻
//     replaceState 清掉，浏览器历史里也不留。
//   - 没设 api_key 时（cfg.APIKey == ""）必须退化成不带片段的地址，不能拼出
//     "#accounts&key=" 这种空尾巴。
func TestPanelURLWithKey(t *testing.T) {
	cases := []struct {
		listen string
		key    string
		want   string
	}{
		{":7863", "abc123", "http://127.0.0.1:7863/panel/#accounts&key=abc123"},
		{":7863", "", "http://127.0.0.1:7863/panel/"},
		{"0.0.0.0:7863", "abc123", "http://127.0.0.1:7863/panel/#accounts&key=abc123"},
		// 密钥里出现片段分隔符时必须转义，否则会被当成另一个字段（&）或直接截断片段（#）。
		{":7863", "a&b", "http://127.0.0.1:7863/panel/#accounts&key=a%26b"},
		{":7863", "a#b", "http://127.0.0.1:7863/panel/#accounts&key=a%23b"},
		// 非 127.0.0.1 的本地地址照旧不改写：面板就在这台机器上。
		{"localhost:9000", "k", "http://localhost:9000/panel/#accounts&key=k"},
	}
	for _, tc := range cases {
		if got := panelURLWithKey(tc.listen, tc.key); got != tc.want {
			t.Errorf("panelURLWithKey(%q, %q) = %q, want %q", tc.listen, tc.key, got, tc.want)
		}
	}
}
