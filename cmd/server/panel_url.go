package main

import (
	"net"
	"strings"
)

// panelURL 拼出本机管理面板的访问地址（托盘菜单「打开面板」用）。
//
// 为什么要自己拼而不是直接用 cfg.Listen：listen 可能是 ":7863"、"0.0.0.0:7863"、
// "[::]:7863" 这类通配写法，浏览器打不开 0.0.0.0 / [::]，必须换成回环地址。
// 浏览器和网关在同一台机器上，所以回环地址永远是对的。
func panelURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		if value, ok := strings.CutPrefix(listen, ":"); ok && value != "" {
			host, port = "", value
		} else {
			host, port = "127.0.0.1", "7863"
		}
	}
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::", "*":
		host = "127.0.0.1"
	}
	if port == "" {
		port = "7863"
	}
	return "http://" + net.JoinHostPort(host, port) + "/panel/"
}

// panelURLWithKey 在 panelURL 的基础上，把面板密钥放进 URL 片段（#accounts&key=…）。
// 托盘菜单「打开面板」用这一支：服务起在本机、浏览器也在本机，双击托盘就能进
// 面板，不必再手工抄一遍 config.json 里的 api_key。
//
// 用**片段**而不是查询参数是有意的：
//   - 片段（# 之后）不进 HTTP 请求，服务器日志里看不到密钥；
//   - 前端拿到后立刻 replaceState 清掉，浏览器历史里不留。
//
// 片段同时带了 `accounts`：面板路由默认视图是「账号池」，带上它让前端解析
// 视图时照旧落到 #accounts。密钥为空时退化为 panelURL 的行为（不加片段）。
// 密钥是本机 config.json 里的随机串，这里只转义片段里非法的 & 和 #。
func panelURLWithKey(listen, key string) string {
	u := panelURL(listen)
	if key == "" {
		return u
	}
	return u + "#accounts&key=" + strings.NewReplacer("&", "%26", "#", "%23").Replace(key)
}
