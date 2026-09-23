package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// FetchAuth 远程认证信息
type FetchAuth struct {
	Type     string // "none" | "basic"
	Username string
	Password string
	Proxy    string // HTTP 代理 URL（含 userinfo 时自动代理认证），如 http://user:pass@host:port
}

// fetch 客户端超时与重试默认值。
// 关键设计：不使用 http.Client.Timeout 作为整体超时（它会覆盖 body 读取，
// 使大仓库镜像在中途被硬性掐断）——改为分层超时：
//   - DialTimeout 建立 TCP 连接（含代理 CONNECT）
//   - TLSHandshakeTimeout TLS 握手
//   - ResponseHeaderTimeout 发出请求到收到响应头
//   - IdleConnTimeout 连接池空闲回收
//   - StallTimeout 传输停滞（连续无数据）上限，通过连接级读写 deadline 实现
//   - TotalTimeout 兜底上限（0 = 不限，默认不限，靠 StallTimeout 保护）
const (
	defaultFetchDialTimeout    = 15 * time.Second
	defaultFetchTLSHandshake   = 15 * time.Second
	defaultFetchRespHeader     = 60 * time.Second
	defaultFetchIdleConn       = 90 * time.Second
	defaultFetchStallTimeout   = 120 * time.Second
	defaultFetchAttemptTimeout = 0 // 0 = 不限制单次尝试总时长
	defaultFetchMaxAttempts    = 3
	defaultFetchRetryBaseDelay = 1 * time.Second
	defaultFetchRetryMaxDelay  = 30 * time.Second
)

// FetchOptions 控制 fetch 客户端的超时与重试行为。零值使用默认值。
type FetchOptions struct {
	// StallTimeout 传输停滞上限：超过该时长未收到任何字节即判定失败并重试。
	StallTimeout time.Duration
	// ResponseHeaderTimeout 等待响应头的上限。
	ResponseHeaderTimeout time.Duration
	// TotalTimeout 单次尝试的整体上限（0 = 不限，由 StallTimeout 保护）。
	TotalTimeout time.Duration
	// MaxAttempts 最大尝试次数（含首次），<=1 表示不重试。
	MaxAttempts int
	// RetryBaseDelay / RetryMaxDelay 重试退避区间。
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
}

func (o FetchOptions) withDefaults() FetchOptions {
	out := o
	if out.StallTimeout <= 0 {
		out.StallTimeout = defaultFetchStallTimeout
	}
	if out.ResponseHeaderTimeout <= 0 {
		out.ResponseHeaderTimeout = defaultFetchRespHeader
	}
	if out.TotalTimeout < 0 {
		out.TotalTimeout = 0
	}
	if out.MaxAttempts <= 0 {
		out.MaxAttempts = defaultFetchMaxAttempts
	}
	if out.RetryBaseDelay <= 0 {
		out.RetryBaseDelay = defaultFetchRetryBaseDelay
	}
	if out.RetryMaxDelay <= 0 {
		out.RetryMaxDelay = defaultFetchRetryMaxDelay
	}
	return out
}

// stallReader 在每次读到数据时调用 touch，用于把 StallTimeout 实现为
// 「连续无数据」上限（而非总时长），从而不会掐断长时间但持续有数据的大仓库传输。
type stallReader struct {
	r     io.Reader
	touch func()
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 && s.touch != nil {
		s.touch()
	}
	return n, err
}

// FetchResult fetch 操作结果
type FetchResult struct {
	ObjectsWritten int   // 写入 loose store 的对象数
	RefsUpdated    int   // 创建/更新的 ref 数
	RefsDeleted    int   // 删除的 ref 数(本地有但远程无)
	UpToDate       bool  // true = 无新对象(want 全被 have 覆盖)
	Wants          int   // want oid 数
	Haves          int   // have oid 数
	PackSize       int64 // pack 数据字节数
}

// FetchRemote 从 remoteURL 拉取仓库到 repoRoot（smart-http upload-pack 客户端），
// 使用默认超时与重试策略。详见 FetchRemoteWithOptions。
func FetchRemote(remoteURL, repoRoot string, auth *FetchAuth) (*FetchResult, error) {
	return FetchRemoteWithOptions(remoteURL, repoRoot, auth, FetchOptions{})
}

// FetchRemoteWithOptions 与 FetchRemote 相同，但可指定超时与重试策略。
// 失败（可重试错误）时按指数退避 + jitter 重试，最多 opts.MaxAttempts 次。
func FetchRemoteWithOptions(remoteURL, repoRoot string, auth *FetchAuth, opts FetchOptions) (*FetchResult, error) {
	o := opts.withDefaults()

	var lastErr error
	for attempt := 1; attempt <= o.MaxAttempts; attempt++ {
		start := time.Now()
		result, err := fetchOnce(remoteURL, repoRoot, auth, o)
		if err == nil {
			if attempt > 1 {
				slog.Info("fetch succeeded after retry", "url", remoteURL, "attempt", attempt)
			}
			return result, nil
		}
		lastErr = err

		if !isRetryableFetchError(err) || attempt == o.MaxAttempts {
			if attempt > 1 {
				slog.Warn("fetch giving up", "url", remoteURL, "attempt", attempt, "error", err)
			}
			return nil, err
		}

		delay := retryDelay(attempt, o.RetryBaseDelay, o.RetryMaxDelay)
		slog.Warn("fetch attempt failed, retrying",
			"url", remoteURL, "attempt", attempt, "maxAttempts", o.MaxAttempts,
			"retryIn", delay.Round(time.Millisecond), "error", err, "elapsedMs", time.Since(start).Milliseconds())
		retrySleep(delay)
	}
	// 理论不可达：循环在 attempt == MaxAttempts 时返回
	return nil, lastErr
}

// retrySleep 执行重试前的等待。测试可替换为零延迟，避免用例被真实退避拖慢。
var retrySleep = time.Sleep

// retryDelay 计算第 attempt 次尝试失败后的退避时间（指数增长 + 抖动），结果不超过 max。
// 抖动范围 [0.75d, 1.25d]，避免多仓库同时重试造成尖峰。
func retryDelay(attempt int, base, max time.Duration) time.Duration {
	d := base << (attempt - 1) // base * 2^(attempt-1)
	if d <= 0 || d > max {
		d = max
	}
	// 抖动：0.75d + [0, 0.5d] → [0.75d, 1.25d]，再夹到 max
	jitter := time.Duration(rand.Int63n(int64(d/2 + 1)))
	out := d - d/4 + jitter
	if out > max {
		out = max
	}
	return out
}

// isRetryableFetchError 判定错误是否值得重试：
// 网络类（超时/连接失败/中断/EOF）与 5xx 可重试；4xx（认证/不存在）与
// 数据格式错误（pack 损坏）不重试——重试也不会成功。
func isRetryableFetchError(err error) bool {
	if err == nil {
		return false
	}
	// 显式标记
	var fe *fetchError
	if errors.As(err, &fe) {
		return fe.retryable
	}
	// 网络类超时/中断
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	// 连接层错误（不同平台措辞不同，保守匹配常见片段）
	for _, frag := range []string{
		"connection refused", "connection reset", "broken pipe",
		"no such host", "i/o timeout", "TLS handshake timeout",
		"EOF", "use of closed network connection",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}

// wrapTransportErr 包装传输层错误：超时/连接类可重试。
func wrapTransportErr(what string, err error) error {
	return &fetchError{
		msg:       what,
		err:       err,
		retryable: true, // 传输层失败（含 ctx 取消导致的停滞超时）均可重试
	}
}

// httpStatusErr 按 HTTP 状态码归类：5xx 与 429 可重试，其余 4xx 永久失败。
func httpStatusErr(what string, status int) error {
	retry := status >= 500 || status == http.StatusTooManyRequests
	return &fetchError{msg: fmt.Sprintf("%s status %d", what, status), retryable: retry}
}

// fetchError 是带「可重试」标记的 fetch 错误。
type fetchError struct {
	msg       string
	retryable bool
	err       error
}

func (e *fetchError) Error() string {
	if e.err != nil {
		return e.msg + ": " + e.err.Error()
	}
	return e.msg
}

func (e *fetchError) Unwrap() error { return e.err }

func retryableErr(format string, args ...any) error {
	return &fetchError{msg: fmt.Sprintf(format, args...), retryable: true}
}

func permanentErr(format string, args ...any) error {
	return &fetchError{msg: fmt.Sprintf(format, args...), retryable: false}
}

// fetchOnce 执行一次完整的 fetch 尝试（无重试逻辑）。
// StallTimeout 通过 ctx 取消实现：传输期间连续无数据超过该时长即主动中止本次尝试。
func fetchOnce(remoteURL, repoRoot string, auth *FetchAuth, o FetchOptions) (*FetchResult, error) {
	fetchStart := time.Now()
	remoteURL = strings.TrimRight(remoteURL, "/")

	ctx := context.Background()
	if o.TotalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.TotalTimeout)
		defer cancel()
	}
	ctx, cancelStall := context.WithCancel(ctx)
	defer cancelStall()

	// 停滞看门狗：stallTimer 每收到数据重置；到期未重置即取消请求。
	stallTimer := time.AfterFunc(o.StallTimeout, cancelStall)
	touch := func() { stallTimer.Reset(o.StallTimeout) }
	defer stallTimer.Stop()

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   defaultFetchDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   defaultFetchTLSHandshake,
		ResponseHeaderTimeout: o.ResponseHeaderTimeout,
		IdleConnTimeout:       defaultFetchIdleConn,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if auth != nil && auth.Proxy != "" {
		proxyURL, err := url.Parse(auth.Proxy)
		if err != nil {
			return nil, fmt.Errorf("fetch: invalid proxy URL: %w", err)
		}
		if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
			return nil, fmt.Errorf("fetch: proxy URL must be http or https, got %q", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	// 注意：不设置 client.Timeout —— 整体超时会掐断大仓库的 body 读取，
	// 改由 StallTimeout（停滞检测）与 ResponseHeaderTimeout 保护。
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()

	remoteRefs, serverCaps, err := fetchInfoRefs(ctx, client, remoteURL, auth, touch)
	if err != nil {
		slog.Warn("fetch info/refs failed", "url", remoteURL, "error", err)
		return nil, err
	}

	if len(remoteRefs) == 0 {
		slog.Info("fetch empty remote", "url", remoteURL)
		return &FetchResult{UpToDate: true}, nil
	}

	rs := NewRefStore(repoRoot)
	localRefsList, _ := rs.List()
	localRefs := make(map[string]Oid)
	localOidSet := make(map[Oid]bool)
	for _, r := range localRefsList {
		if r.Name == "HEAD" {
			continue
		}
		if !strings.HasPrefix(r.Name, "refs/") {
			continue
		}
		localRefs[r.Name] = r.Oid
		localOidSet[r.Oid] = true
	}

	var wantOids []Oid
	wantSet := make(map[Oid]bool)
	for name, oid := range remoteRefs {
		if name == "HEAD" {
			continue
		}
		if !wantSet[oid] {
			wantSet[oid] = true
			wantOids = append(wantOids, oid)
		}
	}

	allLocal := true
	for _, oid := range wantOids {
		if !localOidSet[oid] {
			allLocal = false
			break
		}
	}
	if allLocal {
		slog.Info("fetch up-to-date", "wants", len(wantOids), "haves", len(localOidSet))
		return &FetchResult{UpToDate: true, Wants: len(wantOids), Haves: len(localOidSet)}, nil
	}

	var haveOids []Oid
	haveSet := make(map[Oid]bool)
	for _, oid := range localRefs {
		if !haveSet[oid] {
			haveSet[oid] = true
			haveOids = append(haveOids, oid)
		}
	}

	clientCaps := "ofs-delta"
	if strings.Contains(serverCaps, "side-band-64k") {
		clientCaps = "side-band-64k ofs-delta"
	}
	useSideband := strings.Contains(clientCaps, "side-band-64k")

	var reqBuf bytes.Buffer
	pw := NewPktWriter(&reqBuf)
	for i, oid := range wantOids {
		if i == 0 {
			pw.WritePktString(fmt.Sprintf("want %s %s\n", oid, clientCaps))
		} else {
			pw.WritePktString(fmt.Sprintf("want %s\n", oid))
		}
	}
	pw.WriteFlush()
	for _, oid := range haveOids {
		pw.WritePktString(fmt.Sprintf("have %s\n", oid))
	}
	pw.WritePktString("done\n")

	postReq, err := http.NewRequestWithContext(ctx, "POST", remoteURL+"/git-upload-pack", &reqBuf)
	if err != nil {
		return nil, fmt.Errorf("fetch: new upload-pack request: %w", err)
	}
	postReq.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	postReq.Header.Set("Accept", "application/x-git-upload-pack-result")
	if auth != nil && auth.Type == "basic" {
		postReq.SetBasicAuth(auth.Username, auth.Password)
	}
	postResp, err := client.Do(postReq)
	if err != nil {
		slog.Warn("fetch upload-pack request failed", "error", err)
		return nil, wrapTransportErr("fetch upload-pack request", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		slog.Warn("fetch upload-pack non-200", "status", postResp.StatusCode)
		return nil, httpStatusErr("fetch upload-pack", postResp.StatusCode)
	}

	pr := NewPktReader(&stallReader{r: postResp.Body, touch: touch})
	firstPayload, isFlush, err := pr.ReadPkt()
	if err != nil {
		return nil, fmt.Errorf("fetch: read first response: %w", err)
	}
	if isFlush {
		return nil, fmt.Errorf("fetch: expected NAK/ACK, got flush")
	}
	firstLine := string(firstPayload)
	if firstLine != "NAK\n" && !strings.HasPrefix(firstLine, "ACK ") {
		return nil, fmt.Errorf("fetch: expected NAK or ACK, got %q", firstPayload)
	}

	// 流式解码：sideband 下用 io.Pipe 把 ch1 数据流喂给解码器（不缓存整个 pack）；
	// 非 sideband 下直接把响应体交给解码器（尾部 flush-pkt 由解码器容忍）。
	store := NewObjectStore(repoRoot)
	var packSrc io.Reader
	if useSideband {
		pr2, pw := io.Pipe()
		go func() {
			defer pw.Close()
			for {
				payload, isFlush, err := pr.ReadPkt()
				if err != nil {
					pw.CloseWithError(fmt.Errorf("read sideband: %w", err))
					return
				}
				if isFlush {
					return
				}
				if len(payload) < 1 {
					continue
				}
				switch payload[0] {
				case SidebandPack:
					touch()
					if _, err := pw.Write(payload[1:]); err != nil {
						return
					}
				case SidebandProgress:
					slog.Debug("remote progress", "message", strings.TrimSpace(string(payload[1:])))
				case SidebandError:
					pw.CloseWithError(fmt.Errorf("remote error: %s", string(payload[1:])))
					return
				}
			}
		}()
		packSrc = pr2
	} else {
		// 非 sideband：响应体可能只是 flush-pkt（无可发送对象）或直接是 pack
		peek := bufio.NewReader(&stallReader{r: postResp.Body, touch: touch})
		if head, err := peek.Peek(4); err == nil && string(head) == PktFlush {
			slog.Info("fetch up-to-date", "wants", len(wantOids), "haves", len(haveOids))
			return &FetchResult{UpToDate: true, Wants: len(wantOids), Haves: len(haveOids)}, nil
		}
		packSrc = peek
	}

	counter := &countingReader{}
	dec := NewPackDecoder(io.TeeReader(packSrc, counter), store)
	objectsWritten, err := dec.DecodeTo(store)
	if err != nil {
		if isUpToDate(err) {
			slog.Info("fetch up-to-date", "wants", len(wantOids), "haves", len(haveOids))
			return &FetchResult{UpToDate: true, Wants: len(wantOids), Haves: len(haveOids)}, nil
		}
		slog.Warn("fetch decode pack failed", "error", err)
		return nil, fmt.Errorf("fetch decode pack: %w", err)
	}
	if objectsWritten == 0 {
		slog.Info("fetch up-to-date", "wants", len(wantOids), "haves", len(haveOids))
		return &FetchResult{UpToDate: true, Wants: len(wantOids), Haves: len(haveOids)}, nil
	}
	packSize := counter.n
	if packSize == 0 {
		packSize = int64(objectsWritten)
	}

	var updates []RefUpdate
	for name, remoteOid := range remoteRefs {
		if name == "HEAD" {
			continue
		}
		if localOid, ok := localRefs[name]; ok {
			updates = append(updates, RefUpdate{Name: name, OldOid: localOid, NewOid: remoteOid})
		} else {
			updates = append(updates, RefUpdate{Name: name, OldOid: ZeroOid, NewOid: remoteOid})
		}
	}
	for name, localOid := range localRefs {
		if _, ok := remoteRefs[name]; !ok {
			updates = append(updates, RefUpdate{Name: name, OldOid: localOid, NewOid: ZeroOid})
		}
	}

	results, err := rs.Update(updates)
	if err != nil {
		return nil, fmt.Errorf("fetch: update refs: %w", err)
	}
	refsUpdated := 0
	refsDeleted := 0
	for i, u := range updates {
		if i < len(results) && results[i].Ok {
			if u.NewOid == ZeroOid {
				refsDeleted++
			} else {
				refsUpdated++
			}
		} else if i < len(results) {
			slog.Warn("fetch ref update failed", "ref", u.Name, "reason", results[i].Reason)
		}
	}

	if headOid, ok := remoteRefs["HEAD"]; ok && headOid.Valid() && !headOid.IsZero() {
		for name, oid := range remoteRefs {
			if strings.HasPrefix(name, "refs/heads/") && oid == headOid {
				rs.SetHead(name)
				break
			}
		}
	} else {
		head, _ := rs.Head()
		if head != "" {
			if _, err := rs.Get(head); err != nil {
				currentRefs, _ := rs.List()
				for _, r := range currentRefs {
					if strings.HasPrefix(r.Name, "refs/heads/") {
						rs.SetHead(r.Name)
						break
					}
				}
			}
		}
	}

	duration := time.Since(fetchStart).Milliseconds()
	slog.Info("fetch done",
		"wants", len(wantOids), "haves", len(haveOids), "objects", objectsWritten,
		"packSize", packSize, "durationMs", duration)

	return &FetchResult{
		ObjectsWritten: objectsWritten,
		RefsUpdated:    refsUpdated,
		RefsDeleted:    refsDeleted,
		UpToDate:       false,
		Wants:          len(wantOids),
		Haves:          len(haveOids),
		PackSize:       packSize,
	}, nil
}

// fetchInfoRefs 获取并解析 smart-http ref advertisement，返回 remoteRefs 与 serverCaps。
func fetchInfoRefs(ctx context.Context, client *http.Client, remoteURL string, auth *FetchAuth, touch func()) (map[string]Oid, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", remoteURL+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: new info/refs request: %w", err)
	}
	req.Header.Set("Accept", "application/x-git-upload-pack-advertisement")
	if auth != nil && auth.Type == "basic" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", wrapTransportErr("fetch info/refs request", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", httpStatusErr("fetch info/refs", resp.StatusCode)
	}

	pr := NewPktReader(&stallReader{r: resp.Body, touch: touch})
	svc, isFlush, err := pr.ReadPkt()
	if err != nil {
		return nil, "", fmt.Errorf("fetch: read service frame: %w", err)
	}
	if isFlush {
		return nil, "", fmt.Errorf("fetch: unexpected flush for service frame")
	}
	if !strings.Contains(string(svc), "git-upload-pack") {
		return nil, "", fmt.Errorf("fetch: unexpected service frame %q", svc)
	}
	_, isFlush, err = pr.ReadPkt()
	if err != nil {
		return nil, "", fmt.Errorf("fetch: read flush after service: %w", err)
	}
	if !isFlush {
		return nil, "", fmt.Errorf("fetch: expected flush after service frame")
	}

	remoteRefs := make(map[string]Oid)
	var serverCaps string
	firstRef := true
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			return nil, "", fmt.Errorf("fetch: read ref advertisement: %w", err)
		}
		if isFlush {
			break
		}
		line := string(payload)
		var main, capPart string
		if i := strings.IndexByte(line, 0); i >= 0 {
			main = line[:i]
			capPart = line[i+1:]
		} else {
			main = line
		}
		main = strings.TrimRight(main, "\n")
		if firstRef {
			serverCaps = strings.TrimRight(capPart, "\n")
			firstRef = false
		}
		if strings.Contains(main, "capabilities^{}") {
			continue
		}
		fields := strings.Fields(main)
		if len(fields) >= 2 {
			remoteRefs[fields[1]] = Oid(fields[0])
		}
	}
	return remoteRefs, serverCaps, nil
}

// countingReader 作为 io.Writer 统计（Tee 过来的）pack 字节数。
type countingReader struct{ n int64 }

func (c *countingReader) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// isUpToDate 判断解码失败是否属于「无 pack 数据」（服务端仅回 NAK+flush）。
func isUpToDate(err error) bool {
	return strings.Contains(err.Error(), "too short") || strings.Contains(err.Error(), "read header") || errors.Is(err, io.EOF)
}
