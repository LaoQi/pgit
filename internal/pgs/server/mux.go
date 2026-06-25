package server

import (
	"bufio"
	"bytes"
	"log"
	"net"
	"time"
)

var sshPrefix = []byte("SSH-")

type peekConn struct {
	r  *bufio.Reader
	nc net.Conn
}

func (c *peekConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *peekConn) Write(p []byte) (int, error)        { return c.nc.Write(p) }
func (c *peekConn) Close() error                       { return c.nc.Close() }
func (c *peekConn) LocalAddr() net.Addr                { return c.nc.LocalAddr() }
func (c *peekConn) RemoteAddr() net.Addr               { return c.nc.RemoteAddr() }
func (c *peekConn) SetDeadline(t time.Time) error      { return c.nc.SetDeadline(t) }
func (c *peekConn) SetReadDeadline(t time.Time) error  { return c.nc.SetReadDeadline(t) }
func (c *peekConn) SetWriteDeadline(t time.Time) error { return c.nc.SetWriteDeadline(t) }

// MuxListener satisfies net.Listener by piping pre-created conns from a channel.
// Each accepted connection is fed into a channel; HTTP and SSH handlers consume
// from it after protocol detection.
type MuxServer struct {
	ln       net.Listener
	enableSSH bool
	ssh      *SSHHandler
	http     *HTTPHandler
}

func NewMuxServer(ln net.Listener, enableSSH bool, ssh *SSHHandler, http *HTTPHandler) *MuxServer {
	return &MuxServer{ln: ln, enableSSH: enableSSH, ssh: ssh, http: http}
}

func (m *MuxServer) Serve() error {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return err
		}
		go m.handleConn(conn)
	}
}

func (m *MuxServer) handleConn(conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("mux: handleConn panic: %v", r)
			conn.Close()
		}
	}()

	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	prefix, err := br.Peek(4)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}

	pc := &peekConn{r: br, nc: conn}

	if m.enableSSH && bytes.HasPrefix(prefix, sshPrefix) {
		m.ssh.HandleConn(pc)
	} else {
		m.http.HandleConn(pc)
	}
}
