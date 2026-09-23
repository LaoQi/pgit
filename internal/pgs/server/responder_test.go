package server

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 包装后的 ResponseWriter 必须透传 Flusher（否则 SSE/流式响应无法即时刷新）。
func TestResponseStatusWriterFlusher(t *testing.T) {
	rec := httptest.NewRecorder() // *httptest.ResponseRecorder 实现 http.Flusher
	w := &responseStatusWriter{ResponseWriter: rec, status: http.StatusOK}

	f, ok := interface{}(w).(http.Flusher)
	if !ok {
		t.Fatal("responseStatusWriter should implement http.Flusher")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("chunk"))
	f.Flush()
	if !rec.Flushed {
		t.Error("Flush should be forwarded to the underlying writer")
	}
}

// 透传 io.ReaderFrom（大响应走快速路径）。
func TestResponseStatusWriterReaderFrom(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &responseStatusWriter{ResponseWriter: rec, status: http.StatusOK}

	n, err := io.Copy(w, strings.NewReader("hello world"))
	if err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	if n != 11 || rec.Body.String() != "hello world" {
		t.Errorf("copied %d bytes, body = %q", n, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (Write 应隐式提交 200)", rec.Code)
	}
}

// 重复 WriteHeader 不应覆盖首次记录的状态（与 net/http 语义一致）。
func TestResponseStatusWriterIgnoresDuplicateWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &responseStatusWriter{ResponseWriter: rec, status: http.StatusOK}

	w.WriteHeader(http.StatusCreated)
	w.WriteHeader(http.StatusInternalServerError) // 应被忽略
	if w.status != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.status, http.StatusCreated)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("underlying status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

// Write 未显式 WriteHeader 时应记录 200（此前 status 会停留在初始值而漏记）。
func TestResponseStatusWriterRecordsImplicit200(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &responseStatusWriter{ResponseWriter: rec, status: http.StatusOK}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if !w.wrote {
		t.Error("Write should mark the response as written")
	}
}

// fakeHijacker / 无 Hijack 能力时应返回 ErrNotSupported 而不是 panic。
func TestResponseStatusWriterHijackUnsupported(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &responseStatusWriter{ResponseWriter: rec, status: http.StatusOK}
	if _, _, err := w.Hijack(); err != http.ErrNotSupported {
		t.Errorf("Hijack err = %v, want http.ErrNotSupported", err)
	}
	if err := w.Push("/x", nil); err != http.ErrNotSupported {
		t.Errorf("Push err = %v, want http.ErrNotSupported", err)
	}
	if w.Unwrap() == nil {
		t.Error("Unwrap should return the underlying writer")
	}
}

// hijackableWriter 提供 Hijack 能力，用于验证透传。
type hijackableWriter struct {
	http.ResponseWriter
	conn net.Conn
}

func (h *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

func TestResponseStatusWriterHijackForwarded(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	w := &responseStatusWriter{ResponseWriter: &hijackableWriter{conn: c1}, status: http.StatusOK}
	conn, _, err := w.Hijack()
	if err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	if conn != c1 {
		t.Error("Hijack should return the underlying connection")
	}
}

// 中间件端到端：requestLogger 包装后 handler 仍能 Flush 且状态码被正确记录。
func TestRequestLoggerPreservesFlusher(t *testing.T) {
	var flushed bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("data"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			flushed = true
		}
	})
	h := requestLogger(inner)

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	if !bytes.Equal(body, []byte("data")) {
		t.Errorf("body = %q", body)
	}
	if !flushed {
		t.Error("handler should see http.Flusher through requestLogger")
	}
}
