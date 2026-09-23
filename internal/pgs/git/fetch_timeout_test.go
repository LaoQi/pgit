package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 错误分类：网络/5xx/429/停滞 → 可重试；4xx/数据损坏 → 永久失败。
func TestIsRetryableFetchError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"http 500", httpStatusErr("upload-pack", 500), true},
		{"http 503", httpStatusErr("upload-pack", 503), true},
		{"http 429", httpStatusErr("upload-pack", 429), true},
		{"http 404", httpStatusErr("upload-pack", 404), false},
		{"http 401", httpStatusErr("info/refs", 401), false},
		{"http 403", httpStatusErr("info/refs", 403), false},
		{"transport timeout", wrapTransportErr("req", errors.New("net/http: request canceled")), true},
		{"EOF", io.ErrUnexpectedEOF, true},
		{"eof", io.EOF, true},
		{"connection refused", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), true},
		{"no such host", errors.New("dial tcp: lookup bad.invalid: no such host"), true},
		{"i/o timeout", errors.New("read tcp: i/o timeout"), true},
		{"pack corrupted", fmt.Errorf("fetch: decode pack: %w", errors.New("pack: trailer sha1 mismatch")), false},
		{"delta corrupt", fmt.Errorf("ref-delta apply: %w", errors.New("delta: copy out of range")), false},
		{"permanent marked", permanentErr("bad config"), false},
		{"retryable marked", retryableErr("flaky"), true},
	}
	for _, c := range cases {
		if got := isRetryableFetchError(c.err); got != c.want {
			t.Errorf("%s: isRetryableFetchError = %v, want %v", c.name, got, c.want)
		}
	}
}

// 退避：指数增长且封顶，并带抖动（不超过上限）。
func TestRetryDelayBackoff(t *testing.T) {
	base, max := 1*time.Second, 8*time.Second
	prev := time.Duration(0)
	for attempt := 1; attempt <= 6; attempt++ {
		d := retryDelay(attempt, base, max)
		if d < 0 {
			t.Fatalf("attempt %d: negative delay %v", attempt, d)
		}
		if d > max {
			t.Errorf("attempt %d: delay %v exceeds max %v", attempt, d, max)
		}
		if attempt <= 3 && d < prev {
			t.Errorf("attempt %d: delay %v < previous %v (should grow)", attempt, d, prev)
		}
		prev = d
	}
	// 抖动应使同一 attempt 的结果不完全相同
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[retryDelay(4, base, max)] = true
	}
	if len(seen) < 2 {
		t.Error("retryDelay should include jitter (all values identical)")
	}
}

// 重试：前 N 次失败后成功；总尝试次数与退避次数正确。
func TestFetchRemoteRetries(t *testing.T) {
	var attempts atomic.Int32
	var requestLog []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		requestLog = append(requestLog, r.URL.Path)
		if attempts.Load() < 3 {
			http.Error(w, "boom", http.StatusInternalServerError) // 5xx → 可重试
			return
		}
		// 第三次：返回空远程（无 refs）→ 视为 up-to-date，fetch 成功
		pw := NewPktWriter(w)
		_ = pw.WritePktString("# service=git-upload-pack\n")
		_ = pw.WriteFlush()
		_ = pw.WriteFlush()
	}))
	defer srv.Close()

	dir := t.TempDir()
	res, err := FetchRemoteWithOptions(srv.URL, dir, nil, FetchOptions{MaxAttempts: 5, RetryBaseDelay: time.Millisecond})
	if err != nil {
		t.Fatalf("fetch should succeed after retries: %v", err)
	}
	if !res.UpToDate {
		t.Errorf("result = %+v, want UpToDate", res)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// 永久失败（4xx）不重试：只尝试一次。
func TestFetchRemoteDoesNotRetry4xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := FetchRemoteWithOptions(srv.URL, dir, nil, FetchOptions{MaxAttempts: 5, RetryBaseDelay: time.Millisecond})
	if err == nil {
		t.Fatal("401 should fail")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to mention 401", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (4xx must not be retried)", got)
	}
	if isRetryableFetchError(err) {
		t.Error("401 should be classified as permanent")
	}
}

// 耗尽重试：返回最后一次错误。
func TestFetchRemoteExhaustsRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := FetchRemoteWithOptions(srv.URL, dir, nil, FetchOptions{MaxAttempts: 3, RetryBaseDelay: time.Millisecond})
	if err == nil {
		t.Fatal("should fail after exhausting attempts")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// 停滞检测：服务端挂在响应头阶段不返回 → ResponseHeaderTimeout 生效并重试。
func TestFetchRemoteStallOnResponseHeader(t *testing.T) {
	var attempts atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		<-release // 不写响应头，制造停滞
	}))
	defer func() { close(release); srv.Close() }()

	dir := t.TempDir()
	start := time.Now()
	_, err := FetchRemoteWithOptions(srv.URL, dir, nil, FetchOptions{
		MaxAttempts:           2,
		RetryBaseDelay:        time.Millisecond,
		ResponseHeaderTimeout: 200 * time.Millisecond,
		StallTimeout:          200 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("stalled server should cause failure")
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("attempts = %d, want >= 2 (stall should be retried)", got)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v, timeout not effective", elapsed)
	}
}

// firstBlobOid 从 pack 中解出第一个对象的 oid（供构造 ref advertisement）。
func firstBlobOid(pack []byte) Oid {
	objs, err := NewPackDecoder(bytes.NewReader(pack)).Decode()
	if err != nil || len(objs) == 0 {
		return ZeroOid
	}
	return objs[0].Oid()
}

// 关键回归：正常但耗时超过旧的 5 分钟整体超时的传输不应被掐断。
// 用「响应头慢但持续有数据」的 pack 流验证 StallTimeout 只在无数据时才触发。
func TestFetchRemoteSlowButProgressingNotAborted(t *testing.T) {
	// 用不可压缩内容确保 pack 足够大，分帧后总耗时超过 StallTimeout
	pack := buildPackIncompressible(t, 4, 8192)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "info/refs") {
			pw := NewPktWriter(w)
			_ = pw.WritePktString("# service=git-upload-pack\n")
			_ = pw.WriteFlush()
			// 必须是非 HEAD 的 ref（HEAD 会被 fetch 跳过），否则 wantOids 为空
			_ = pw.WritePktString(fmt.Sprintf("%s refs/heads/master\x00side-band-64k ofs-delta\n", firstBlobOid(pack)))
			_ = pw.WriteFlush()
			return
		}
		// upload-pack：NAK + sideband 分帧慢慢吐 pack（每帧间隔 >0 但整体远超 stall 阈值）
		pw := NewPktWriter(w)
		_ = pw.WritePktString("NAK\n")
		fw := w.(http.Flusher)
		fw.Flush()
		const chunk = 512
		for off := 0; off < len(pack); off += chunk {
			end := off + chunk
			if end > len(pack) {
				end = len(pack)
			}
			frame := append([]byte{SidebandPack}, pack[off:end]...)
			if err := pw.WritePkt(frame); err != nil {
				return
			}
			fw.Flush()
			time.Sleep(20 * time.Millisecond) // 每帧有进展，但总量耗时 > StallTimeout
		}
		_ = pw.WriteFlush()
	}))
	defer srv.Close()

	dir := t.TempDir()
	res, err := FetchRemoteWithOptions(srv.URL, dir, nil, FetchOptions{
		MaxAttempts:    1,
		StallTimeout:   100 * time.Millisecond,
		RetryBaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("slow-but-progressing transfer should complete: %v", err)
	}
	_ = res // 关键是未被 StallTimeout 掐断（pack 已完整接收并解码）
}
