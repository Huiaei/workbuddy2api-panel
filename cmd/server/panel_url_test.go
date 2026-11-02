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
