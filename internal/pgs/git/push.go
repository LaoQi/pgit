package git

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RefSpec 是一条推送命令。Oid 为 ZeroOid 表示删除该 ref；
// Force 为真时命令行带 "+" 前缀（允许覆盖上游非快进）。
// 中转仓库的准入层保证只会传入快进/新建/删除，不使用 Force。
type RefSpec struct {
	Ref   string
	Oid   Oid
	Force bool
}

// PushRefResult 是单个 ref 的推送结果。
type PushRefResult struct {
	Ref    string `json:"ref"`
	OldOid Oid    `json:"oldOid"`
	NewOid Oid    `json:"newOid"`
	Ok     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// PushResult 是一次 push 的结果汇总。
type PushResult struct {
	Refs        []PushRefResult // 与传入 specs 同序、同数量
	RemoteRefs  map[string]Oid  // 推送前的上游 ref 广告
	Caps        string          // 上游 capabilities
	ObjectsSent int             // 本次实际打包发送的对象数
	PackSize    int64           // pack 字节数（0 = 未发送 pack）
	UpToDate    bool            // 无需任何 ref 变更（未发 POST）
}

// Failed 返回未达到目标状态的 ref 结果（上游 ng 或未报告状态）。
func (r *PushResult) Failed() []PushRefResult {
	if r == nil {
		return nil
	}
	out := make([]PushRefResult, 0)
	for _, ref := range r.Refs {
		if !ref.Ok {
			out = append(out, ref)
		}
	}
	return out
}

// AllOK 表示所有 ref 都已达到目标状态（含"上游本已一致"的 no-op）。
func (r *PushResult) AllOK() bool {
	if r == nil {
		return false
	}
	for _, ref := range r.Refs {
		if !ref.Ok {
			return false
		}
	}
	return true
}

// PushRemote 把本地 repoRoot 的若干 ref 推送到 remoteURL（smart-http receive-pack 客户端）。
//
// 语义与 git push 一致：
//   - spec.Oid 非零：把 ref 更新为（或新建为）该 oid，old 取上游广告的值（CAS）
//   - spec.Oid 为零：删除上游 ref（要求上游广告含 delete-refs）
//   - 上游本已一致 / 上游本无该 ref 的删除：记为 ok 的 no-op，不发命令也不发 pack
//
// 待发送对象 = 目标 oid 可达但上游 refs（含各命令 old oid）不可达的部分，
// 因此调用方必须保证传进来的 oid 对上游而言是"可增量补齐"的（中转仓库由基线准入保证）。
// 上游不支持 ofs-delta 时退化为全量对象编码；支持时复用与 clone 相同的出向 delta 编码。
//
// 部分 ref 被上游拒绝（ng）不算函数错误，通过 PushResult.Failed() 检查；
// pack 不可用 / 协议异常 / 传输失败才返回 error（可重试标记沿用 fetch 的分类）。
func PushRemote(remoteURL, repoRoot string, specs []RefSpec, auth *FetchAuth, opts FetchOptions) (*PushResult, error) {
	o := opts.withDefaults()

	var lastResult *PushResult
	var lastErr error
	for attempt := 1; attempt <= o.MaxAttempts; attempt++ {
		start := time.Now()
		result, err := pushOnce(remoteURL, repoRoot, specs, auth, o)
		if err == nil {
			if attempt > 1 {
				slog.Info("push succeeded after retry", "url", remoteURL, "attempt", attempt)
			}
			return result, nil
		}
		lastResult, lastErr = result, err
		if !isRetryableFetchError(err) || attempt == o.MaxAttempts {
			if attempt > 1 {
				slog.Warn("push giving up", "url", remoteURL, "attempt", attempt, "error", err)
			}
			return result, err
		}
		delay := retryDelay(attempt, o.RetryBaseDelay, o.RetryMaxDelay)
		slog.Warn("push attempt failed, retrying",
			"url", remoteURL, "attempt", attempt, "maxAttempts", o.MaxAttempts,
			"retryIn", delay.Round(time.Millisecond), "error", err,
			"elapsedMs", time.Since(start).Milliseconds())
		retrySleep(delay)
	}
	return lastResult, lastErr
}

// pushCmd 是一条实际要发给上游的命令（old 取上游广告值，保证 CAS 语义）。
type pushCmd struct {
	spec RefSpec
	old  Oid
}

func pushOnce(remoteURL, repoRoot string, specs []RefSpec, auth *FetchAuth, o FetchOptions) (*PushResult, error) {
	remoteURL = strings.TrimRight(remoteURL, "/")

	session, err := newRemoteSession(auth, o)
	if err != nil {
		return nil, permanentErr("push: %v", err)
	}
	defer session.Close()

	remoteRefs, caps, err := fetchRefAdvertisement(session.Ctx, session.Client, remoteURL, "git-receive-pack", auth, session.Touch)
	if err != nil {
		return nil, err
	}
	result := &PushResult{RemoteRefs: remoteRefs, Caps: caps}
	if !hasCap(caps, "report-status") {
		// 没有 report-status 就无法得知逐 ref 结果，宁可报错也不要误判成功。
		return result, permanentErr("push: upstream does not advertise report-status")
	}

	// 组装命令；无需变更的 ref 直接产出 ok 结果（no-op）。
	cmds := make([]pushCmd, 0, len(specs))
	results := make([]PushRefResult, 0, len(specs))
	cmdIndex := make(map[string]int, len(specs))
	for _, sp := range specs {
		old := refOid(remoteRefs, sp.Ref)
		r := PushRefResult{Ref: sp.Ref, OldOid: old, NewOid: sp.Oid}
		switch {
		case sp.Ref == "":
			r.Reason = "empty ref name"
		case sp.Oid.IsZero() && old.IsZero():
			r.Ok, r.Reason = true, "already absent upstream"
		case sp.Oid == old:
			r.Ok, r.Reason = true, "already up-to-date upstream"
		case sp.Oid.IsZero() && !hasCap(caps, "delete-refs"):
			r.Reason = "upstream does not advertise delete-refs"
		default:
			cmdIndex[sp.Ref] = len(results)
			cmds = append(cmds, pushCmd{spec: sp, old: old})
		}
		results = append(results, r)
	}
	result.Refs = results
	if len(cmds) == 0 {
		result.UpToDate = true
		slog.Info("push up-to-date", "url", remoteURL, "refs", len(specs))
		return result, nil
	}

	// 待发送对象：目标 oid 可达且上游不可达的部分。
	// haves = 上游全部 ref oid + 各命令 old oid（本地没有的上游对象由 WalkReachable 自动跳过）。
	store := NewObjectStore(repoRoot)
	haveSet := make(map[Oid]bool)
	var haves, roots []Oid
	addHave := func(oid Oid) {
		if !oid.IsZero() && !haveSet[oid] {
			haveSet[oid] = true
			haves = append(haves, oid)
		}
	}
	for _, oid := range remoteRefs {
		addHave(oid)
	}
	for _, c := range cmds {
		addHave(c.old)
		if !c.spec.Oid.IsZero() {
			roots = append(roots, c.spec.Oid)
		}
	}
	var metas []ObjectMeta
	if len(roots) > 0 {
		metas, err = WalkReachable(store, roots, haves, nil)
		if err != nil {
			return result, permanentErr("push: walk reachable: %v", err)
		}
	}
	useDelta := hasCap(caps, "ofs-delta")
	useSideband := hasCap(caps, "side-band-64k")
	clientCaps := pushClientCaps(caps)

	// 请求体流式生成：命令 pkt-line + flush + pack（边编码边发，不驻留内存）。
	bodyReader, bodyWriter := io.Pipe()
	var encMu sync.Mutex
	var encErr error
	encDone := make(chan struct{})
	packCounter := &countingReader{}
	go func() {
		defer close(encDone)
		err := writePushBody(bodyWriter, cmds, clientCaps, store, metas, useDelta, packCounter)
		if err != nil {
			_ = bodyWriter.CloseWithError(err)
		} else {
			_ = bodyWriter.Close()
		}
		encMu.Lock()
		encErr = err
		encMu.Unlock()
	}()

	req, err := newRemoteRequest(session.Ctx, "POST", remoteURL+"/git-receive-pack", bodyReader, auth,
		"application/x-git-receive-pack-result")
	if err != nil {
		bodyReader.CloseWithError(err)
		<-encDone
		return result, permanentErr("push: new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")

	slog.Info("push sending", "url", remoteURL, "refs", len(cmds), "objects", len(metas))
	resp, derr := session.Client.Do(req)
	// 请求体已写完或请求已失败：解除写端阻塞并等待编码 goroutine 收尾（避免泄漏与竞态）。
	_ = bodyReader.CloseWithError(io.ErrClosedPipe)
	<-encDone
	encMu.Lock()
	buildErr := encErr
	encMu.Unlock()
	// 分类顺序：传输失败 → HTTP 状态码 → 请求体构建错误。
	// 必须先看响应再构建错误：上游/代理可能不读请求体就早响应（过载 502/503），
	// 此时编码 goroutine 会被管道关闭打断（buildErr != nil），但真实原因是 HTTP 状态，
	// 按 5xx 可重试分类才能触发重试；若按构建错误分类会误判为永久失败。
	if derr != nil {
		return result, wrapTransportErr("push request", derr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result, httpStatusErr("push", resp.StatusCode)
	}
	if buildErr != nil {
		// 200 但请求体没能完整生成/发送（连接中途断开、本地对象缺失等）。
		// 上游不可能据此完成 ref 更新（pack 不完整 → unpack 失败；ref 更新是 CAS），
		// 重放安全，按可重试处理。
		return result, retryableErr("push: build request body: %v", buildErr)
	}

	// 响应：report-status（sideband 时经 ch1 重组后再解析 pkt-line）。
	touched := &stallReader{r: resp.Body, touch: session.Touch}
	var statusSrc io.Reader = touched
	if useSideband {
		statusSrc = NewSidebandReader(NewPktReader(touched))
	}
	unpackErr, statuses, err := parseReportStatus(NewPktReader(statusSrc))
	if err != nil {
		return result, retryableErr("push: %v", err)
	}
	for _, c := range cmds {
		i := cmdIndex[c.spec.Ref]
		if st, ok := statuses[c.spec.Ref]; ok {
			result.Refs[i].Ok = st.Ok
			result.Refs[i].Reason = st.Reason
			continue
		}
		result.Refs[i].Ok = false
		if unpackErr != "" {
			result.Refs[i].Reason = "unpack failed: " + unpackErr
		} else {
			result.Refs[i].Reason = "no status reported by upstream"
		}
	}
	result.ObjectsSent = len(metas)
	result.PackSize = packCounter.n
	if unpackErr != "" {
		return result, retryableErr("push: upstream unpack failed: %s", unpackErr)
	}
	var ng []string
	for _, r := range result.Refs {
		if !r.Ok {
			ng = append(ng, fmt.Sprintf("%s (%s)", r.Ref, r.Reason))
		}
	}
	slog.Info("push done", "url", remoteURL, "objects", len(metas), "bytes", packCounter.n,
		"rejected", strings.Join(ng, ", "))
	return result, nil
}

// refOid 取广告中的 ref oid；该 ref 不存在（或 oid 非法）时返回 ZeroOid。
// 注意不能用裸 map 取值：缺失键得到的是空 Oid("")，既不等于 ZeroOid 也不是合法 oid，
// 会污染后续的 oid 比较与可达性遍历。
func refOid(refs map[string]Oid, name string) Oid {
	if oid, ok := refs[name]; ok && oid.Valid() {
		return oid
	}
	return ZeroOid
}

// pushClientCaps 按上游能力挑选客户端 caps。
func pushClientCaps(serverCaps string) string {
	caps := []string{"report-status"}
	if hasCap(serverCaps, "side-band-64k") {
		caps = append(caps, "side-band-64k")
	}
	if hasCap(serverCaps, "ofs-delta") {
		caps = append(caps, "ofs-delta")
	}
	return strings.Join(caps, " ")
}

// writePushBody 生成 receive-pack 请求体：命令 pkt-line（首行带 caps）+ flush + pack。
// pack 为空（纯删除/无对象可发）时不写 pack。
func writePushBody(w io.Writer, cmds []pushCmd, caps string, store ObjectStore, metas []ObjectMeta, useDelta bool, counter io.Writer) error {
	pw := NewPktWriter(w)
	for i, c := range cmds {
		prefix := ""
		if c.spec.Force {
			prefix = "+"
		}
		var b strings.Builder
		b.WriteString(prefix)
		b.WriteString(c.old.String())
		b.WriteByte(' ')
		b.WriteString(c.spec.Oid.String())
		b.WriteByte(' ')
		b.WriteString(c.spec.Ref)
		if i == 0 {
			b.WriteByte(0)
			b.WriteString(caps)
		}
		b.WriteString("\n")
		if err := pw.WritePktString(b.String()); err != nil {
			return fmt.Errorf("write command %s: %w", c.spec.Ref, err)
		}
	}
	if err := pw.WriteFlush(); err != nil {
		return fmt.Errorf("write flush: %w", err)
	}
	if len(metas) == 0 {
		return nil
	}
	out := w
	if counter != nil {
		out = io.MultiWriter(w, counter)
	}
	if err := encodePackForPush(store, metas, out, useDelta); err != nil {
		return fmt.Errorf("encode pack: %w", err)
	}
	return nil
}

// encodePackForPush 按上游能力编码 pack：支持 ofs-delta 时复用 clone 的出向 delta 编码，
// 否则退化为全量对象（上游未广告 ofs-delta 时不能出现 delta 对象）。
func encodePackForPush(store ObjectStore, metas []ObjectMeta, w io.Writer, allowDelta bool) error {
	if allowDelta {
		return encodePack(store, metas, w)
	}
	enc := NewPackEncoder(w)
	if err := enc.WriteHeader(len(metas)); err != nil {
		return err
	}
	for _, m := range metas {
		obj, err := store.Read(m.Oid)
		if err != nil {
			return fmt.Errorf("read %s: %w", m.Oid, err)
		}
		if err := enc.WriteObject(obj); err != nil {
			return fmt.Errorf("pack obj %s: %w", m.Oid, err)
		}
	}
	return enc.WriteTrailer()
}

// parseReportStatus 解析 receive-pack 的 report-status：
// 首帧 "unpack ok|ng <reason>"，随后每 ref "ok <ref>" / "ng <ref> <reason>"，末尾 flush。
// 返回 unpack 错误串（ok 时为空）与逐 ref 结果。
func parseReportStatus(pr *PktReader) (string, map[string]PushRefResult, error) {
	first, isFlush, err := pr.ReadPkt()
	if err != nil {
		return "", nil, fmt.Errorf("read unpack status: %w", err)
	}
	if isFlush {
		return "", nil, fmt.Errorf("unexpected flush before unpack status")
	}
	line := strings.TrimRight(string(first), "\n")
	if !strings.HasPrefix(line, "unpack ") {
		return "", nil, fmt.Errorf("unexpected response %q", strings.TrimRight(string(first), "\n"))
	}
	unpackErr := strings.TrimSpace(strings.TrimPrefix(line, "unpack "))
	if unpackErr == "ok" {
		unpackErr = ""
	}

	statuses := make(map[string]PushRefResult)
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			return unpackErr, statuses, fmt.Errorf("read report-status: %w", err)
		}
		if isFlush {
			break
		}
		l := strings.TrimRight(string(payload), "\n")
		switch {
		case strings.HasPrefix(l, "ok "):
			ref := strings.TrimSpace(strings.TrimPrefix(l, "ok "))
			statuses[ref] = PushRefResult{Ref: ref, Ok: true}
		case strings.HasPrefix(l, "ng "):
			ref, reason, _ := strings.Cut(strings.TrimPrefix(l, "ng "), " ")
			statuses[ref] = PushRefResult{Ref: ref, Ok: false, Reason: strings.TrimSpace(reason)}
		default:
			slog.Debug("push: unknown report-status line", "line", l)
		}
	}
	return unpackErr, statuses, nil
}
