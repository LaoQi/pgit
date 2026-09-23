package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"pgit/internal/pgs"
)

// 启动一个最小 mux（HTTP only），返回地址与关闭函数。
func startMux(t *testing.T, gitRoot string) (addr string, mux *MuxServer, shutdown func()) {
	t.Helper()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: gitRoot})
	t.Cleanup(func() { pgs.ReposManager = nil })

	settings := &pgs.Setting{}
	h := NewHTTPHandler(pgs.ReposManager, settings, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := NewMuxServer(ln, false, nil, h)
	go func() { _ = m.Serve() }()
	return ln.Addr().String(), m, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = m.Shutdown(ctx)
	}
}

// 共享 http.Server 下多个连接可并发处理（此前每连接一个 server 且存在并发写字段的竞态）。
func TestMuxHandlesConcurrentConnections(t *testing.T) {
	addr, _, shutdown := startMux(t, t.TempDir())
	defer shutdown()

	client := &http.Client{Timeout: 5 * time.Second}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://" + addr + "/healthz")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("status = %d", resp.StatusCode)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent request failed: %v", err)
	}
}

// HTTP 连接由 http.Server 负责关闭：请求结束后连接可被服务端回收
// （此前 singleConnListener.Close 不关闭底层连接，keep-alive 连接滞留）。
func TestMuxClosesConnectionsOnShutdown(t *testing.T) {
	addr, _, shutdown := startMux(t, t.TempDir())

	// 建立 keep-alive 连接并完成一次请求
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", addr); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "200 OK") {
		t.Fatalf("unexpected response: %q", buf[:n])
	}

	shutdown()

	// 关闭后：连接应被服务端关闭（读到 EOF/重置），且新连接被拒
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Error("keep-alive connection should be closed after shutdown")
	}
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Error("listener should not accept new connections after shutdown")
	}
}

// Shutdown 幂等，且重复调用不 panic。
func TestMuxShutdownIdempotent(t *testing.T) {
	_, m, shutdown := startMux(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := m.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("first shutdown: %v", err)
	}
	if err := m.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second shutdown: %v", err)
	}
	_ = shutdown
}

// Serve 在监听器关闭后返回 net.ErrClosed（供 main 判断是否正常退出）。
func TestMuxServeReturnsOnListenerClose(t *testing.T) {
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	h := NewHTTPHandler(pgs.ReposManager, &pgs.Setting{}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := NewMuxServer(ln, false, nil, h)

	errCh := make(chan error, 1)
	go func() { errCh <- m.Serve() }()

	time.Sleep(100 * time.Millisecond)
	_ = ln.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("Serve should return an error after listener close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after listener close")
	}
}

// 优雅关闭会等待进行中的请求完成（不硬切）。
func TestMuxShutdownWaitsForInflightRequest(t *testing.T) {
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })

	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release // 阻塞直到测试放行
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})
	handler := NewHTTPHandler(pgs.ReposManager, &pgs.Setting{}, nil)
	m := NewMuxServer(newTestListener(t), false, nil, handler)
	m.httpSrv.Handler = mux // 用一个可控的慢 handler

	go func() { _ = m.Serve() }()
	addr := m.ln.Addr().String()

	respCh := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			respCh <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		respCh <- string(b)
	}()

	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- m.Shutdown(ctx) }()

	// 请求仍被阻塞 → Shutdown 不应提前返回
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before inflight request completed: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release) // 放行请求

	select {
	case got := <-respCh:
		if got != "done" {
			t.Fatalf("inflight request got %q, want done", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inflight request did not complete")
	}
	select {
	case err := <-shutdownDone:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("shutdown error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish after request completed")
	}
}

func newTestListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}
