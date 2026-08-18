//go:build windows

package proxydial

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// registryProxy Windows 注册表 IE 系统代理检测:
// HKCU\...\Internet Settings{ProxyEnable,ProxyServer};ProxyServer 支持
// "host:port" 或分协议 "http=h:p;https=h:p;socks=h:p" 两种形态。
func registryProxy() string {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()

	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		return ""
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil || server == "" {
		return ""
	}

	// 分协议形态:优先 https=,其次 http=,兜底 socks=
	if strings.Contains(server, "=") {
		m := map[string]string{}
		for _, seg := range strings.Split(server, ";") {
			if kv := strings.SplitN(strings.TrimSpace(seg), "=", 2); len(kv) == 2 {
				m[strings.ToLower(kv[0])] = kv[1]
			}
		}
		for _, key := range []string{"https", "http", "socks"} {
			if v, ok := m[key]; ok && v != "" {
				if key == "socks" {
					return "socks5://" + v
				}
				return key + "://" + v
			}
		}
		return ""
	}
	return "http://" + server
}
