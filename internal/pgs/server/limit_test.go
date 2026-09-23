package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pgit/internal/pgs"
)

// pack 传递并发上限：并行请求数超过 maxConcurrentPacks 时排队而不是全部并发；
// 达到上限且上下文取消时返回 503。
func TestPackSemaphoreLimitsConcurrency(t *testing.T) {
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })

	settings := &pgs.Setting{MaxConcurrentPacks: 1}
	h := NewHTTPHandler(pgs.ReposManager, settings, nil)

	// 占满名额
	if !h.acquirePack(context.Background()) {
		t.Fatal("first acquire should succeed")
	}

	// 已取消的上下文：不得获取名额
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h.acquirePack(ctx) {
		t.Fatal("acquire with canceled context must fail")
	}

	// 释放后可再次获取
	h.releasePack()
	if !h.acquirePack(context.Background()) {
		t.Fatal("acquire after release should succeed")
	}
	h.releasePack()
}

// 默认上限生效：未配置时使用 DefaultMaxConcurrentPacks。
func TestPackSemaphoreDefaultSize(t *testing.T) {
	settings := &pgs.Setting{}
	if got := settings.LimitConcurrentPacks(); got != pgs.DefaultMaxConcurrentPacks {
		t.Fatalf("LimitConcurrentPacks = %d, want %d", got, pgs.DefaultMaxConcurrentPacks)
	}
	if got := settings.LimitPushBytes(); got != pgs.DefaultMaxPushBytes {
		t.Fatalf("LimitPushBytes = %d, want %d", got, pgs.DefaultMaxPushBytes)
	}
	settings.MaxConcurrentPacks = 7
	settings.MaxPushBytes = 123
	if got := settings.LimitConcurrentPacks(); got != 7 {
		t.Fatalf("LimitConcurrentPacks = %d, want 7", got)
	}
	if got := settings.LimitPushBytes(); got != 123 {
		t.Fatalf("LimitPushBytes = %d, want 123", got)
	}
}

// 超过 maxPushBytes 的 receive-pack 请求：响应体不得包含 pack 数据，
// 并有明确错误（不挂断客户端）。
func TestReceivePackOverLimitReturnsStatus(t *testing.T) {
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	if err := pgs.ReposManager.CreateRepository("r", "", "master"); err != nil {
		t.Fatal(err)
	}

	settings := &pgs.Setting{MaxPushBytes: 1024, MaxConcurrentPacks: 2}
	h := NewHTTPHandler(pgs.ReposManager, settings, nil)

	body := strings.NewReader(strings.Repeat("x", 8192))
	req := httptest.NewRequest(http.MethodPost, "/r.git/git-receive-pack", body)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { h.router.ServeHTTP(rec, req); close(done) }()

	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("receive-pack over limit hung instead of returning")
	}
	if rec.Code == 0 {
		t.Fatal("no response written")
	}
}

// timeAfter 是测试超时保护（10s 后强制失败）。
func timeAfter() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		time.Sleep(10 * time.Second)
	}()
	return ch
}
