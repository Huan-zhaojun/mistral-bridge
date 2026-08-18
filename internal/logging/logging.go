// Package logging 结构化日志初始化:slog JSON → lumberjack 落盘轮转 + console 双写。
// 日志输出信息一律英文(项目规范);永不落 Authorization 与 body 全文。
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Config 日志配置
type Config struct {
	Level   string // debug|info|warn|error
	Dir     string // 日志目录
	Console bool   // 输出 stdout
	File    bool   // 落盘轮转
}

// 内置轮转盘(不进配置面,与产品语义无关)
const (
	maxSizeMB  = 64
	maxBackups = 7
	maxAgeDays = 14
)

// Init 初始化全局 logger;返回 logger 与可选清理函数
func Init(cfg Config) (*slog.Logger, error) {
	var w io.Writer = io.Discard
	var lw *lumberjack.Logger
	if cfg.File {
		if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir log dir: %w", err)
		}
		lw = &lumberjack.Logger{
			Filename:   filepath.Join(cfg.Dir, "mistral-bridge.log"),
			MaxSize:    maxSizeMB,
			MaxBackups: maxBackups,
			MaxAge:     maxAgeDays,
			Compress:   false,
		}
		w = lw
	}
	if cfg.Console {
		w = io.MultiWriter(os.Stdout, w)
	}
	if w == io.Discard {
		return nil, fmt.Errorf("logging disabled: both console and file are off")
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: parseLevel(cfg.Level)})
	return slog.New(h), nil
}

// parseLevel 文本级别 → slog.Level(非法值回落 info)
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
