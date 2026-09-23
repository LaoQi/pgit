package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

var sshPrefix = []byte("SSH-")

// 接入层超时与上限（防慢连接占用资源）。
const (
	// muxPeekTimeout 协议探测（peek 4 字节）的最长等待：防空连接。
	muxPeekTimeout = 10 * time.Second
	// httpReadHeaderTimeout 读完请求头的最长时间（slowloris 防护）。
	httpReadHeaderTimeout = 15 * time.Second
	// httpIdleTimeout keep-alive 空闲连接的最长存活。
	httpIdleTimeout = 120 * time.Second
	// sshHandshakeTimeout SSH 握手（版本交换 + 认证）的最长时间。
	sshHandshakeTimeout = 30 * time.Second //nolint:unused // 阶段 4-2 使用
)

type peekConn struct {
	r  *bufio.Reader
	nc net.Conn
}

func (c *peekConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *peekConn) Write(p []byte) (int, error)       { return c.nc.Write(p) }
func (c *peekConn) Close() error                      { return c.nc.Close() }
func (c *peekConn) LocalAddr() net.Addr               { return c.nc.LocalAddr() }
func (c *peekConn) RemoteAddr() net.Addr              { return c.nc.RemoteAddr() }
func (c *peekConn) SetDeadline(t time.Time) error     { return c.nc.SetDeadline(t) }
func (c *peekConn) SetReadDeadline(t time.Time) error { return c.nc.SetReadDeadline(t) }
func (c *peekConn) SetWriteDeadline(t time.Time) error {
	return c.nc.SetWriteDeadline(t)
}

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "pgit-mux" }

// connChanListener 是 net.Listener 实现：把 mux 协议探测后的连接喂给共享的
// http.Server。Close 会关闭投递通道，使阻塞中的 Accept 立刻返回。
type connChanListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newConnChanListener(buf int) *connChanListener {
	return &connChanListener{
		conns:  make(chan net.Conn, buf),
		closed: make(chan struct{}),
	}
}

func (l *connChanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Push 投递一个待处理连接；listener 已关闭时返回 net.ErrClosed 并关闭该连接。
func (l *connChanListener) Push(c net.Conn) error {
	select {
	case l.conns <- c:
		return nil
	case <-l.closed:
		_ = c.Close()
		return net.ErrClosed
	}
}

func (l *connChanListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *connChanListener) Addr() net.Addr { return dummyAddr{} }

// MuxServer 单端口多路复用：Accept 后按前缀分发 SSH / HTTP。
// 所有 HTTP 连接共用同一个 http.Server（含超时配置），便于统一优雅关闭。
type MuxServer struct {
	ln        net.Listener
	enableSSH bool
	ssh       *SSHHandler
	httpH     *HTTPHandler

	httpSrv  *http.Server
	httpLn   *connChanListener
	httpDone chan struct{}

	mu      sync.Mutex
	conns   map[net.Conn]struct{} // 在处理的连接（SSH 与已交付 HTTP 的）
	wg      sync.WaitGroup        // 在处理的连接/请求
	closing bool
}

func NewMuxServer(ln net.Listener, enableSSH bool, ssh *SSHHandler, httpHandler *HTTPHandler) *MuxServer {
	m := &MuxServer{
		ln:        ln,
		enableSSH: enableSSH,
		ssh:       ssh,
		httpH:     httpHandler,
		conns:     map[net.Conn]struct{}{},
	}
	m.httpLn = newConnChanListener(64)
	m.httpDone = make(chan struct{})
	m.httpSrv = &http.Server{
		Handler:           httpHandler.Router(),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
	return m
}

// Serve 启动 HTTP 处理循环并进入 Accept 循环，直到监听器关闭。
func (m *MuxServer) Serve() error {
	// 共享 http.Server：由 connChanListener 提供连接
	go func() {
		defer close(m.httpDone)
		if err := m.httpSrv.Serve(m.httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			slog.Error("http server stopped", "error", err)
		}
	}()

	for {
		conn, err := m.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return net.ErrClosed
			}
			return err
		}
		go m.handleConn(conn)
	}
}

// handleConn 探测协议并把连接交给对应处理器。
func (m *MuxServer) handleConn(conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mux handleConn panic", "panic", r)
			m.forgetConn(conn)
			_ = conn.Close()
		}
	}()

	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(muxPeekTimeout))
	prefix, err := br.Peek(4)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return
	}

	pc := &peekConn{r: br, nc: conn}

	if m.enableSSH && m.ssh != nil && bytes.HasPrefix(prefix, sshPrefix) {
		m.wg.Add(1)
		m.trackConn(pc)
		defer func() {
			m.forgetConn(pc)
			m.wg.Done()
		}()
		m.ssh.HandleConn(pc)
		return
	}

	// HTTP：交付给共享 server（连接生命周期由 http.Server 负责，不在此 wg 计数；
	// 优雅关闭时由 Shutdown 等待活动请求并关闭空闲连接）。
	if err := m.httpLn.Push(pc); err != nil {
		slog.Debug("http connection rejected", "error", err)
	}
}

func (m *MuxServer) trackConn(c net.Conn) {
	m.mu.Lock()
	m.conns[c] = struct{}{}
	m.mu.Unlock()
}

func (m *MuxServer) forgetConn(c net.Conn) {
	m.mu.Lock()
	delete(m.conns, c)
	m.mu.Unlock()
}

// Shutdown 优雅关闭：停止接受新连接 → 等待活动 HTTP 请求与 SSH 会话结束 →
// 超时后强制关闭残留连接。可重复调用。
func (m *MuxServer) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return nil
	}
	m.closing = true
	m.mu.Unlock()

	// 1. 停止接受新连接
	_ = m.ln.Close()
	// 2. HTTP：等待活动请求，关闭空闲 keep-alive 连接
	httpErr := m.httpSrv.Shutdown(ctx)

	// 3. SSH：等待会话结束，超时后强制断开
	sshDone := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(sshDone)
	}()
	select {
	case <-sshDone:
	case <-ctx.Done():
		m.mu.Lock()
		for c := range m.conns {
			_ = c.Close()
		}
		m.mu.Unlock()
		<-sshDone
	}

	// 4. 关闭连接投递通道（若 http.Server 尚未关闭）
	_ = m.httpLn.Close()
	return httpErr
}
