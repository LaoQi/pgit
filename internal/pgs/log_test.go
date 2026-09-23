package pgs

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in    string
		level slog.Level
		debug bool
		err   bool
	}{
		{"", slog.LevelInfo, false, false},
		{"off", slog.LevelInfo, false, false}, // 旧值兼容：off 与 info 等价（历史上即"仅汇总行"）
		{"info", slog.LevelInfo, false, false},
		{"detail", slog.LevelDebug, true, false}, // 旧值兼容：detail = debug
		{"debug", slog.LevelDebug, true, false},
		{"warn", slog.LevelWarn, false, false},
		{"warning", slog.LevelWarn, false, false},
		{"error", slog.LevelError, false, false},
		{"INFO", slog.LevelInfo, false, false}, // 大小写不敏感
		{"verbose", slog.LevelInfo, false, true},
	}
	for _, c := range cases {
		lv, debug, err := parseLogLevel(c.in)
		if (err != nil) != c.err {
			t.Errorf("parseLogLevel(%q) err = %v, want err=%v", c.in, err, c.err)
			continue
		}
		if err != nil {
			continue
		}
		if lv != c.level || debug != c.debug {
			t.Errorf("parseLogLevel(%q) = (%v, %v), want (%v, %v)", c.in, lv, debug, c.level, c.debug)
		}
	}
}

func TestNormalizeLogFormat(t *testing.T) {
	for in, want := range map[string]string{"": FormatText, "text": FormatText, "json": FormatJSON, "JSON": FormatJSON} {
		got, err := normalizeLogFormat(in)
		if err != nil || got != want {
			t.Errorf("normalizeLogFormat(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	if _, err := normalizeLogFormat("xml"); err == nil {
		t.Error("invalid format should error")
	}
}

// SetupLogging 后：slog 默认 logger 生效，且标准库 log 输出被重定向到同一 handler
// （历史 log.Printf 调用点无需改造即成为结构化记录）。
func TestSetupLoggingRedirectsStdLog(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	log.SetOutput(slog.NewLogLogger(handler, slog.LevelInfo).Writer())
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(nil)
		log.SetFlags(log.LstdFlags)
	})

	log.Printf("legacy message %d", 42)

	line := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("std log output is not JSON: %q (%v)", line, err)
	}
	if !strings.Contains(rec["msg"].(string), "legacy message 42") {
		t.Errorf("msg = %v, want it to contain the formatted legacy message", rec["msg"])
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
}

// 默认配置（空 logLevel）下 debug 关闭：slog.Debug 不输出。
func TestSetupLoggingLevelFiltering(t *testing.T) {
	debug, err := SetupLogging(FormatText, "")
	if err != nil {
		t.Fatal(err)
	}
	if debug {
		t.Error("empty logLevel should not enable debug")
	}
	if slog.Default().Enabled(t.Context(), slog.LevelDebug) {
		t.Error("debug should be filtered at info level")
	}
	if !slog.Default().Enabled(t.Context(), slog.LevelInfo) {
		t.Error("info should be enabled at info level")
	}

	debug, err = SetupLogging(FormatText, "detail")
	if err != nil {
		t.Fatal(err)
	}
	if !debug {
		t.Error("'detail' should enable debug (backward compat)")
	}
}
