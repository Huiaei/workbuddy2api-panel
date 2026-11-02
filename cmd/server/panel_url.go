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
