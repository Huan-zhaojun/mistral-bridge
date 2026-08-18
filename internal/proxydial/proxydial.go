// Package proxydial 出口代理链:多格式代理解析、系统代理检测、链式拨号。
// 四态状态机:
// | 自定义代理 | 系统代理 | 行为 |
// |     无     |    无    | 直连 |
// |     无     |    有    | 走系统代理 |
// |     有     |    无    | 只走自定义代理 |
// |     有     |    有    | 链式:先经系统代理到自定义代理,再经自定义代理到上游 |
package proxydial

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// Result 拨号决策产物:交给 http.Transport 使用
type Result struct {
	// DialContext 自定义拨号(自定义代理/链式场景启用;否则为 nil 走 transport 默认)
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// ProxyFunc transport 层代理(仅"无自定义代理 + 系统代理"场景使用)
	ProxyFunc func(req *http.Request) (*url.URL, error)
	// Mode 决策描述(启动日志用):direct|system|custom(direct)|custom(via system)
	Mode string
}

// proxyAddr 已解析的代理地址
type proxyAddr struct {
	scheme   string // http|https|socks5
	host     string // host:port
	user     string
	password string
}

// directDialer 直连拨号器(同时满足 proxy.Dialer 与 proxy.ContextDialer)
var directDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// Build 构建拨号决策:按"自定义代理 × 系统代理"四态组合
func Build(custom string, systemMode string) (*Result, error) {
	sysStr := detectSystemProxy(systemMode) // "" 表示无系统代理

	customAddr, err := parseProxy(custom)
	if err != nil {
		return nil, fmt.Errorf("parse custom proxy: %w", err)
	}
	if sysStr != "" {
		if _, err := parseProxy(sysStr); err != nil {
			return nil, fmt.Errorf("parse system proxy %q: %w", sysStr, err)
		}
	}

	switch {
	case custom == "" && sysStr == "":
		return &Result{Mode: "direct"}, nil

	case custom == "" && sysStr != "":
		// 走系统代理:transport 层标准代理机制(自动处理 CONNECT 与 socks5 方案)
		proxyURL, _ := url.Parse(normalizeScheme(sysStr))
		return &Result{
			ProxyFunc: http.ProxyURL(proxyURL),
			Mode:      "system",
		}, nil

	default:
		// 有自定义代理:全部走 DialContext 手工链路
		var front func(ctx context.Context, network, addr string) (net.Conn, error)
		mode := "custom(direct)"
		if sysStr == "" {
			front = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return directDialer.DialContext(ctx, network, addr)
			}
		} else {
			sysAddr, _ := parseProxy(sysStr)
			front = frontViaProxy(sysAddr) // 经系统代理连到"自定义代理服务器"
			mode = "custom(via system)"
		}
		dc := dialViaProxy(customAddr, front)
		return &Result{DialContext: dc, Mode: mode}, nil
	}
}

// normalizeScheme 无 scheme 的代理串补默认 http://
func normalizeScheme(s string) string {
	if !strings.Contains(s, "://") {
		return "http://" + s
	}
	return s
}

// parseProxy 解析多格式代理串:http(s)://[user:pass@]host:port / socks5://... / 裸 host:port
func parseProxy(s string) (*proxyAddr, error) {
	if s == "" {
		return nil, nil
	}
	u, err := url.Parse(normalizeScheme(s))
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", scheme)
	}
	if scheme == "socks5h" {
		scheme = "socks5" // socks5h = 远端 DNS;x/net/proxy 的 SOCKS5 本身就是远端域名转发
	}
	host := u.Host
	// 补默认端口(多格式兼容的底线,正常用户都会带端口)
	if _, _, err := net.SplitHostPort(host); err != nil {
		def := map[string]string{"http": "80", "https": "443", "socks5": "1080"}[scheme]
		host = net.JoinHostPort(host, def)
	}
	pa := &proxyAddr{scheme: scheme, host: host}
	if u.User != nil {
		pa.user = u.User.Username()
		pa.password, _ = u.User.Password()
	}
	return pa, nil
}

// detectSystemProxy 系统代理检测:环境变量优先,Windows 注册表兜底;off 时强制无
func detectSystemProxy(mode string) string {
	if mode == "off" {
		return ""
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return registryProxy() // 平台相关:Windows 读注册表,其他平台返回 ""
}

// ---------- 拨号组合 ----------

// contextDialer context 版拨号函数签名别名
type contextDialer func(ctx context.Context, network, addr string) (net.Conn, error)

// dialAdapter 将 contextDialer 适配为 x/net proxy.Dialer(无 ctx 版)
type dialAdapter struct{ f contextDialer }

func (d dialAdapter) Dial(network, addr string) (net.Conn, error) {
	return d.f(context.Background(), network, addr)
}

// frontViaProxy 生成"经给定代理连接到任意 addr"的前置拨号器(链式第一跳)
func frontViaProxy(pa *proxyAddr) contextDialer {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch pa.scheme {
		case "socks5":
			d, err := proxy.SOCKS5("tcp", pa.host, paAuth(pa), directDialer)
			if err != nil {
				return nil, err
			}
			return d.Dial(network, addr)
		default: // http/https
			conn, err := directDialer.DialContext(ctx, network, pa.host)
			if err != nil {
				return nil, err
			}
			if pa.scheme == "https" {
				conn = tls.Client(conn, &tls.Config{ServerName: hostOnly(pa.host)})
			}
			if err := connectHTTP(conn, addr, pa); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		}
	}
}

// dialViaProxy 生成"经给定代理连接到最终目标"的 DialContext(链式第二跳或单层)
func dialViaProxy(pa *proxyAddr, front contextDialer) contextDialer {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch pa.scheme {
		case "socks5":
			d, err := proxy.SOCKS5("tcp", pa.host, paAuth(pa), dialAdapter{f: front})
			if err != nil {
				return nil, err
			}
			return d.Dial(network, addr)
		default: // http/https
			conn, err := front(ctx, network, pa.host)
			if err != nil {
				return nil, err
			}
			if pa.scheme == "https" {
				conn = tls.Client(conn, &tls.Config{ServerName: hostOnly(pa.host)})
			}
			if err := connectHTTP(conn, addr, pa); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		}
	}
}

// connectHTTP 在已建连接上执行 HTTP CONNECT 隧道握手并校验 200
func connectHTTP(conn net.Conn, target string, pa *proxyAddr) error {
	var b strings.Builder
	b.WriteString("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n")
	if pa.user != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(pa.user + ":" + pa.password))
		b.WriteString("Proxy-Authorization: Basic " + cred + "\r\n")
	}
	b.WriteString("\r\n")

	conn.SetDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, b.String()); err != nil {
		return fmt.Errorf("proxy connect write: %w", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("proxy connect read: %w", err)
	}
	// 形态校验:HTTP/1.1 200 ...
	parts := strings.SplitN(status, " ", 3)
	if len(parts) < 2 || parts[1] != "200" {
		return fmt.Errorf("proxy connect refused: %s", strings.TrimSpace(status))
	}
	// 消费剩余响应头至空行(CONNECT 200 后无负载,br 无残留风险)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("proxy connect headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return nil
}

// paAuth 代理认证信息(无 user 时返回 nil)
func paAuth(pa *proxyAddr) *proxy.Auth {
	if pa.user == "" {
		return nil
	}
	return &proxy.Auth{User: pa.user, Password: pa.password}
}

// hostOnly 取 host:port 的 host 部分(TLS ServerName 用)
func hostOnly(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return h
}
