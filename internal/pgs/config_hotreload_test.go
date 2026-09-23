package pgs

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 可热加载字段：日志级别/格式、传输上限、凭据即时生效。
func TestHotReloadUpdatesHotFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	writeConfig(t, cfgPath, `{
		"listen": "127.0.0.1:3000",
		"gitRoot": "`+dir+`",
		"logLevel": "info",
		"maxPushBytes": 1024,
		"credentials": {"u": "p1"}
	}`)

	s := &Setting{}
	s.SetConfigPath(cfgPath)
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := s.LimitPushBytes(); got != 1024 {
		t.Fatalf("LimitPushBytes = %d, want 1024", got)
	}

	writeConfig(t, cfgPath, `{
		"listen": "127.0.0.1:3000",
		"gitRoot": "`+dir+`",
		"logLevel": "debug",
		"logFormat": "json",
		"maxPushBytes": 4096,
		"maxConcurrentPacks": 9,
		"credentials": {"u": "p2"}
	}`)
	restart, err := s.HotReload()
	if err != nil {
		t.Fatal(err)
	}
	if len(restart) != 0 {
		t.Errorf("restartNeeded = %v, want none", restart)
	}
	if got := s.LimitPushBytes(); got != 4096 {
		t.Errorf("LimitPushBytes = %d, want 4096 (hot reload)", got)
	}
	if got := s.LimitConcurrentPacks(); got != 9 {
		t.Errorf("LimitConcurrentPacks = %d, want 9 (hot reload)", got)
	}
	if got := s.CredentialsCopy()["u"]; got != "p2" {
		t.Errorf("credentials = %q, want p2 (hot reload)", got)
	}
	// 日志：debug 应已开启（git detail 跟随）
	if s.LogLevel != "debug" || s.LogFormat != "json" {
		t.Errorf("log config = %q/%q, want debug/json", s.LogLevel, s.LogFormat)
	}
}

// 需重启字段：改动被忽略并回报字段名，当前生效值不变。
func TestHotReloadReportsRestartNeeded(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	writeConfig(t, cfgPath, `{"listen":"127.0.0.1:3000","gitRoot":"`+dir+`","httpAuth":false}`)

	s := &Setting{}
	s.SetConfigPath(cfgPath)
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}

	writeConfig(t, cfgPath, `{"listen":"127.0.0.1:3999","gitRoot":"`+dir+`","httpAuth":true,"logLevel":"warn"}`)
	restart, err := s.HotReload()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"listen": true, "httpAuth": true}
	for _, f := range restart {
		delete(want, f)
	}
	if len(want) != 0 {
		t.Errorf("restartNeeded missing %v (got %v)", want, restart)
	}
	// 需重启字段保持不变
	if s.Listen != "127.0.0.1:3000" {
		t.Errorf("listen changed to %q without restart", s.Listen)
	}
	if s.HttpAuth {
		t.Error("httpAuth changed without restart")
	}
	// 可热加载字段仍生效
	if s.LogLevel != "warn" {
		t.Errorf("logLevel = %q, want warn", s.LogLevel)
	}
}

// 配置非法时热加载失败，且当前配置保持不变（不应半应用）。
func TestHotReloadRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	writeConfig(t, cfgPath, `{"listen":"127.0.0.1:3000","gitRoot":"`+dir+`","maxPushBytes":1024}`)

	s := &Setting{}
	s.SetConfigPath(cfgPath)
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}

	writeConfig(t, cfgPath, `{"listen":"127.0.0.1:3000","gitRoot":"`+dir+`","maxPushBytes":-5}`)
	if _, err := s.HotReload(); err == nil {
		t.Fatal("invalid config should fail hot reload")
	}
	if got := s.LimitPushBytes(); got != 1024 {
		t.Errorf("LimitPushBytes = %d, want unchanged 1024", got)
	}

	writeConfig(t, cfgPath, `{not json`)
	if _, err := s.HotReload(); err == nil {
		t.Fatal("malformed JSON should fail hot reload")
	}
	if got := s.LimitPushBytes(); got != 1024 {
		t.Errorf("LimitPushBytes = %d, want unchanged 1024 after malformed reload", got)
	}
}

// 并发读（handler 路径）与热加载写不得竞态。
func TestHotReloadConcurrentWithReads(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	writeConfig(t, cfgPath, `{"listen":"127.0.0.1:3000","gitRoot":"`+dir+`","maxPushBytes":1024,"credentials":{"u":"p"}}`)

	s := &Setting{}
	s.SetConfigPath(cfgPath)
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	for i := 0; i < 6; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				_ = s.LimitPushBytes()
				_ = s.LimitConcurrentPacks()
				_ = s.CredentialsCopy()
				_, _ = s.WebUIConf()
				_ = s.Snapshot()
			}
		}()
	}
	go func() {
		defer func() { done <- struct{}{} }()
		for j := 0; j < 100; j++ {
			_, _ = s.HotReload()
		}
	}()
	for i := 0; i < 7; i++ {
		<-done
	}
}
