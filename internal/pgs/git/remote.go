package git

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 远端 smart-http 客户端公共件：HTTP 客户端构造、停滞看门狗、ref advertisement 获取与解析。
// fetch（镜像拉取）与 push（中转推送）共用同一套超时/认证/代理行为。

// remoteSession 是一次远端 smart-http 会话的传输上下文：
//   - Client：分层超时的 http.Client（无整体 Timeout，避免掐断大仓库传输）
//   - Ctx：带停滞看门狗取消的 context，传给每个请求
//   - Touch：每收到数据时调用，重置停滞计时（连续无数据超过 StallTimeout 即取消）
type remoteSession struct {
	Client *http.Client
	Ctx    context.Context

	stall       time.Duration
	stallTimer  *time.Timer
	cancelStall context.CancelFunc
	cancelTotal context.CancelFunc
	closeIdle   func()
}

// newRemoteSession 构造远端会话。auth 为 nil 时匿名访问（仍遵循环境变量代理）。
func newRemoteSession(auth *FetchAuth, o FetchOptions) (*remoteSession, error) {
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
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
			return nil, fmt.Errorf("proxy URL must be http or https, got %q", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}

	// 注意：不设置 client.Timeout —— 整体超时会掐断大仓库的 body 读取，
	// 改由 StallTimeout（停滞检测）与 ResponseHeaderTimeout 保护。
	s := &remoteSession{Client: &http.Client{Transport: transport}, closeIdle: transport.CloseIdleConnections}

	ctx := context.Background()
	if o.TotalTimeout > 0 {
		ctx, s.cancelTotal = context.WithTimeout(ctx, o.TotalTimeout)
	}
	ctx, s.cancelStall = context.WithCancel(ctx)
	s.Ctx = ctx
	s.stall = o.StallTimeout
	s.stallTimer = time.AfterFunc(o.StallTimeout, s.cancelStall)
	return s, nil
}

// Touch 在每次收到数据时调用，把停滞计时重置为 StallTimeout。
func (s *remoteSession) Touch() {
	if s.stallTimer != nil {
		s.stallTimer.Reset(s.stall)
	}
}

// Close 释放会话资源（可重复调用）。
func (s *remoteSession) Close() {
	if s.stallTimer != nil {
		s.stallTimer.Stop()
	}
	if s.cancelStall != nil {
		s.cancelStall()
	}
	if s.cancelTotal != nil {
		s.cancelTotal()
	}
	if s.closeIdle != nil {
		s.closeIdle()
	}
}

// newRemoteRequest 构造带认证的 smart-http 请求。
func newRemoteRequest(ctx context.Context, method, rawURL string, body io.Reader, auth *FetchAuth, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if auth != nil && auth.Type == "basic" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}
	return req, nil
}

// fetchRefAdvertisement 请求 <remote>/info/refs?service=<service> 并解析 ref 广告，
// 返回 refs 与 server capabilities。service 为 "git-upload-pack" 或 "git-receive-pack"。
func fetchRefAdvertisement(ctx context.Context, client *http.Client, remoteURL, service string, auth *FetchAuth, touch func()) (map[string]Oid, string, error) {
	req, err := newRemoteRequest(ctx, "GET", remoteURL+"/info/refs?service="+service, nil, auth,
		"application/x-"+service+"-advertisement")
	if err != nil {
		return nil, "", fmt.Errorf("new info/refs request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", wrapTransportErr("info/refs request", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", httpStatusErr("info/refs", resp.StatusCode)
	}

	pr := NewPktReader(&stallReader{r: resp.Body, touch: touch})
	svc, isFlush, err := pr.ReadPkt()
	if err != nil {
		return nil, "", fmt.Errorf("read service frame: %w", err)
	}
	if isFlush {
		return nil, "", fmt.Errorf("unexpected flush for service frame")
	}
	if !strings.Contains(string(svc), service) {
		return nil, "", fmt.Errorf("unexpected service frame %q", svc)
	}
	if _, isFlush, err = pr.ReadPkt(); err != nil {
		return nil, "", fmt.Errorf("read flush after service: %w", err)
	} else if !isFlush {
		return nil, "", fmt.Errorf("expected flush after service frame")
	}
	return parseRefAdvertisement(pr, touch)
}

// parseRefAdvertisement 解析 ref advertisement 本体（首行 ref 可带 "\0<caps>"，末尾 flush）。
// caps 取首行的 capabilities；capabilities^{}（空仓库占位）与 peeled tag 行（"<ref>^{}"）
// 不是真实 ref，均跳过。
func parseRefAdvertisement(pr *PktReader, touch func()) (map[string]Oid, string, error) {
	refs := make(map[string]Oid)
	var caps string
	first := true
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			return nil, "", fmt.Errorf("read ref advertisement: %w", err)
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
		if first {
			caps = strings.TrimRight(capPart, "\n")
			first = false
		}
		if strings.Contains(main, "capabilities^{}") {
			continue
		}
		fields := strings.Fields(main)
		if len(fields) < 2 {
			continue
		}
		name := fields[1]
		if strings.HasSuffix(name, "^{}") {
			continue // peeled tag 行：tag 对象本身已在 refs 中，peeled oid 可达
		}
		refs[name] = Oid(fields[0])
	}
	return refs, caps, nil
}

// hasCap 判断 caps 中是否含指定能力（caps 是空格分隔的能力串）。
func hasCap(caps, want string) bool {
	for _, c := range strings.Fields(caps) {
		if c == want {
			return true
		}
	}
	return false
}
