// 桥入口:env 加载 → 日志初始化 → 上游 client 构建 → http.Server 启动 → 信号优雅退出。
// 桥不持有 key:下游请求的 Authorization 原样透传给上游(见 internal/convert/chat.go)。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed 时区库:distroless 容器无 /usr/share/zoneinfo,embed 后 TZ 环境变量全平台生效

	"mistral-bridge/internal/config"
	"mistral-bridge/internal/convert"
	"mistral-bridge/internal/logging"
	"mistral-bridge/internal/proxydial"
	"mistral-bridge/internal/server"
)

// 内置上游防护常量(不开放配置,见计划文档)
const (
	tlsHandshakeTimeout = 15 * time.Second
	idleConnTimeout     = 90 * time.Second
	maxIdleConns        = 256
	sseIdleTimeout      = 300 * time.Second
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		_, _ = os.Stderr.WriteString("config load failed: " + err.Error() + "\n")
		os.Exit(1)
	}
	logger, err := logging.Init(logging.Config(cfg.Log))
	if err != nil {
		_, _ = os.Stderr.WriteString("logging init failed: " + err.Error() + "\n")
		os.Exit(1)
	}

	// 内置工具生效/丢弃 WARN
	if len(cfg.ToolsDropped) > 0 {
		logger.Warn("unknown/conflict builtin tool(s) ignored",
			"dropped", cfg.ToolsDropped,
			"valid", config.ValidBuiltinTools,
		)
	}

	// 代理链决策(四态状态机)
	dialResult, err := proxydial.Build(cfg.Proxy, cfg.SystemProxy)
	if err != nil {
		logger.Error("proxy dial build failed", "err", err.Error())
		os.Exit(1)
	}

	// 上游 client:ResponseHeaderTimeout 给足(非流式可挂 >480s 无字节);
	// 无全局 Timeout(SSE 长流依赖 ctx 和 idle watchdog);不 follow redirect(3xx 原样回)
	transport := &http.Transport{
		Proxy:                 dialResult.ProxyFunc,
		DialContext:           dialResult.DialContext,
		ResponseHeaderTimeout: time.Duration(cfg.UpstreamTimeout * float64(time.Second)),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		IdleConnTimeout:       idleConnTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConns,
		ForceAttemptHTTP2:     true,
	}
	upstreamClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	logger.Info("bridge starting",
		"listen", cfg.Listen,
		"upstream", cfg.UpstreamBase,
		"builtin_tools_effective", cfg.BuiltinTools,
		"proxy_mode", dialResult.Mode,
		"pass_reasoning", cfg.PassReasoning,
		"map_cc_websearch", cfg.MapCCWebSearch,
	)

	chat := convert.NewChatHandler(convert.ChatConfig{
		UpstreamBase:   cfg.UpstreamBase,
		Client:         upstreamClient,
		BuiltinTools:   cfg.BuiltinTools,
		PassReasoning:  cfg.PassReasoning,
		MapCCWebSearch: cfg.MapCCWebSearch,
		SSEIdleTimeout: sseIdleTimeout,
		Logger:         logger,
	})
	handler := server.New(cfg, logger, chat)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 不设 WriteTimeout:SSE 流式需要长连接写;由 SSE idle watchdog 兜底
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err.Error())
			os.Exit(1)
		}
	}()
	logger.Info("bridge listening", "addr", cfg.Listen)

	<-ctx.Done()
	logger.Info("shutdown signal received")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown failed", "err", err.Error())
	}
	logger.Info("bridge stopped")
}

// 编译期确保 slog 被引用(后续 chat handler 真正用上 logger Fields;
// 保留此 import 说明本包日志风格统一走 slog)
var _ = slog.LevelInfo
