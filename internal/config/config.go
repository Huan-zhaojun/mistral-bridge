// Package config mistral-bridge 配置加载:纯环境变量解析 + 默认值 + 校验。
// 零配置可启动:桥不持有任何 key(Authorization 由下游客户端原样透传)。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config 桥运行配置(来源全部为环境变量,均有默认值)
type Config struct {
	Listen          string    // LISTEN_ADDR,监听地址,默认 :8080
	UpstreamBase    string    // UPSTREAM_BASE,上游基址
	UpstreamTimeout float64   // UPSTREAM_TIMEOUT_S,上游整体读超时(秒)
	BuiltinTools    []string  // BUILTIN_TOOLS 解析后的生效集(已过滤去重)
	ToolsDropped    []string  // 被白名单过滤丢弃的项(供启动 WARN)
	PassReasoning   bool      // PASS_REASONING,history reasoning_content 回传
	MapCCWebSearch  bool      // MAP_CC_WEBSEARCH,CC 的 WebSearch function 映射为服务端内置搜索
	Proxy           string    // PROXY,自定义代理(空=不用)
	SystemProxy     string    // SYSTEM_PROXY,auto|off
	Log             LogConfig // 日志配置
}

// LogConfig 日志配置
type LogConfig struct {
	Level   string // LOG_LEVEL:debug|info|warn|error
	Dir     string // LOG_DIR
	Console bool   // LOG_CONSOLE
	File    bool   // LOG_FILE
}

// Default 全默认值(仅三类值需要兜底:未显式设置的 env 均回落到这些值)
func Default() *Config {
	return &Config{
		Listen:          ":8080",
		UpstreamBase:    "https://api.mistral.ai",
		UpstreamTimeout: 600,
		BuiltinTools:    nil,
		ToolsDropped:    nil,
		PassReasoning:   true,
		MapCCWebSearch:  true,
		Proxy:           "",
		SystemProxy:     "auto",
		Log: LogConfig{
			Level:   "info",
			Dir:     "logs",
			Console: true,
			File:    true,
		},
	}
}

// Load 从环境变量加载并与默认值合并(无配置文件,零配置即默认全集)
func Load() (*Config, error) {
	cfg := Default()

	if v := strings.TrimSpace(os.Getenv("LISTEN_ADDR")); v != "" {
		cfg.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("UPSTREAM_BASE")); v != "" {
		cfg.UpstreamBase = v
	}
	if v := strings.TrimSpace(os.Getenv("UPSTREAM_TIMEOUT_S")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("UPSTREAM_TIMEOUT_S must be a positive number, got %q", v)
		}
		cfg.UpstreamTimeout = f
	}

	// 内置工具默认注入集(中文输入法友好解析)
	tools, dropped := ParseBuiltinTools(os.Getenv("BUILTIN_TOOLS"))
	cfg.BuiltinTools = tools
	cfg.ToolsDropped = dropped

	if v, ok := parseBoolEnv("PASS_REASONING"); ok {
		cfg.PassReasoning = v
	}
	if v, ok := parseBoolEnv("MAP_CC_WEBSEARCH"); ok {
		cfg.MapCCWebSearch = v
	}
	if v := strings.TrimSpace(os.Getenv("PROXY")); v != "" {
		cfg.Proxy = v
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("SYSTEM_PROXY"))); v != "" {
		if v != "auto" && v != "off" {
			return nil, fmt.Errorf("SYSTEM_PROXY must be auto|off, got %q", v)
		}
		cfg.SystemProxy = v
	}

	if v := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))); v != "" {
		cfg.Log.Level = v
	}
	if v := strings.TrimSpace(os.Getenv("LOG_DIR")); v != "" {
		cfg.Log.Dir = v
	}
	if v, ok := parseBoolEnv("LOG_CONSOLE"); ok {
		cfg.Log.Console = v
	}
	if v, ok := parseBoolEnv("LOG_FILE"); ok {
		cfg.Log.File = v
	}

	if strings.TrimSpace(cfg.UpstreamBase) == "" {
		return nil, fmt.Errorf("UPSTREAM_BASE must not be empty")
	}
	if strings.TrimSpace(cfg.Listen) == "" {
		return nil, fmt.Errorf("LISTEN_ADDR must not be empty")
	}
	if !cfg.Log.Console && !cfg.Log.File {
		return nil, fmt.Errorf("at least one of LOG_CONSOLE or LOG_FILE must be enabled")
	}
	return cfg, nil
}

// parseBoolEnv 解析布尔 env:1/true/yes/on 为真;0/false/no/off 为假;空/非法返回 false,false
func parseBoolEnv(key string) (bool, bool) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return false, false
	}
	switch v {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}
