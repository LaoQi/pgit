package pgs

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
)

// 日志级别（配置 logLevel 取值）：
//
//	debug  —— 逐条 want/have/object/delta（旧值 "detail" 的别名）
//	info   —— 默认：阶段汇总、请求日志、同步结果
//	warn   —— 仅告警与错误
//	error  —— 仅错误
//
// 兼容旧值：""（空）与 "off" 等价于 info（历史上即"仅汇总行"），"detail" 等价于 debug。
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// 日志格式（配置 logFormat 取值）。
const (
	FormatText = "text"
	FormatJSON = "json"
)

// parseLogLevel 把配置值映射为 slog 级别，同时报告是否为 debug（供 git 包注入）。
func parseLogLevel(v string) (slog.Level, bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "info":
		return slog.LevelInfo, false, nil
	case "detail", "debug":
		return slog.LevelDebug, true, nil
	case "warn", "warning":
		return slog.LevelWarn, false, nil
	case "error":
		return slog.LevelError, false, nil
	default:
		return slog.LevelInfo, false, fmt.Errorf("invalid logLevel: %q (want debug|info|warn|error)", v)
	}
}

// normalizeLogFormat 校验并归一化日志格式。
func normalizeLogFormat(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	default:
		return "", fmt.Errorf("invalid logFormat: %q (want text|json)", v)
	}
}

// SetupLogging 按配置初始化全局日志：slog 默认 logger（text/json），并把标准库 log
// 的输出重定向到同一 handler —— 这样历史代码里的 log.Printf 无需逐个改造即成为结构化记录。
// 返回是否启用 debug 级（供 git 包 detail 日志注入）。
func SetupLogging(format string, level string) (debug bool, err error) {
	lv, debug, err := parseLogLevel(level)
	if err != nil {
		return false, err
	}
	f, err := normalizeLogFormat(format)
	if err != nil {
		return false, err
	}

	opts := &slog.HandlerOptions{Level: lv}
	var handler slog.Handler
	if f == FormatJSON {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))

	// 标准库 log → slog（Info）：既有 log.Printf 调用点自动结构化，无需改动
	stdLogger := slog.NewLogLogger(handler, slog.LevelInfo)
	log.SetOutput(stdLogger.Writer())
	log.SetFlags(0)
	log.SetPrefix("")

	return debug, nil
}
