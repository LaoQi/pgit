package git

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// LogLevel 控制 upload-pack 详细日志级别（由 pgs 配置注入，避免 pgs/git → pgs 循环依赖）。
type LogLevel int

const (
	LogOff    LogLevel = iota // 默认：仅汇总行
	LogDetail                 // 逐条 want/have/object/delta
)

// logLevel 是进程级日志级别（pgs.Reload 经 SetLogLevel 注入）。
// 用 atomic 保存：配置注入与并发请求（ServeUploadPack/ReceivePack）可能同时发生。
var logLevel atomic.Int32

// maxReceivePackBytes 是单次 receive-pack 可接受的 pack 上限（安全兜底）。
// 由 pgs 配置（maxPushBytes）经 SetMaxReceivePackBytes 注入。
var maxReceivePackBytes atomic.Int64

func init() { maxReceivePackBytes.Store(defaultMaxReceivePackBytes) }

// defaultMaxReceivePackBytes 是未配置时的兜底上限（4 GiB）。
const defaultMaxReceivePackBytes int64 = 4 << 30

// SetMaxReceivePackBytes 设置单次 receive-pack 的 pack 大小上限（<=0 表示用默认值）。
func SetMaxReceivePackBytes(v int64) {
	if v <= 0 {
		v = defaultMaxReceivePackBytes
	}
	maxReceivePackBytes.Store(v)
}

// SetLogLevel 设置 git 包日志级别（pgs.Reload 调用），并发安全。
func SetLogLevel(l LogLevel) { logLevel.Store(int32(l)) }

// currentLogLevel 读取当前日志级别。
func currentLogLevel() LogLevel { return LogLevel(logLevel.Load()) }

// debugf 输出 debug 级结构化日志（仅 detail/debug 配置下生效）。
func debugf(msg string, args ...any) {
	if currentLogLevel() >= LogDetail {
		slog.Debug(msg, args...)
	}
}

// upload-pack v0 capabilities
const uploadPackCaps = "thin-pack side-band-64k ofs-delta no-progress include-tag"

// receive-pack v0 capabilities
const receivePackCaps = "report-status side-band-64k delete-refs"

// capsForService 返回指定 service 的 capabilities 字符串。
func capsForService(service string) (string, error) {
	switch service {
	case "git-upload-pack":
		return uploadPackCaps, nil
	case "git-receive-pack":
		return receivePackCaps, nil
	default:
		return "", fmt.Errorf("unknown service %q", service)
	}
}

// AdvertiseRefs 生成 v0 ref advertisement（pkt-line 字节序列）。
// 不含 smart-http 的 "# service=" 前缀帧，仅 ref advertisement 本体。
// service 为 "git-upload-pack" 或 "git-receive-pack"。
func AdvertiseRefs(repoRoot string, service string) ([]byte, error) {
	caps, err := capsForService(service)
	if err != nil {
		return nil, err
	}

	rs := NewRefStore(repoRoot)
	refs, err := rs.List()
	if err != nil {
		return nil, fmt.Errorf("advertise: list refs: %w", err)
	}

	// 分离 HEAD 与实际分支 refs。空仓库判定基于实际 refs（不含 HEAD）：
	// InitBare 创建的仓库有 HEAD symref 但无分支，List() 返回 [HEAD]，
	// 若不分离会误发 "<ZeroOid> HEAD" 而非标准 capabilities^{}。
	var head *Ref
	actualRefs := make([]Ref, 0, len(refs))
	for i := range refs {
		if refs[i].Name == "HEAD" {
			head = &refs[i]
		} else {
			actualRefs = append(actualRefs, refs[i])
		}
	}

	var buf bytes.Buffer
	pw := NewPktWriter(&buf)

	// 空仓库（无实际分支 ref）：<ZeroOid> capabilities^{}\x00<caps>\n + flush
	// 与 cgit 一致，upload-pack/receive-pack 空仓库均发 capabilities^{}，不发 HEAD。
	if len(actualRefs) == 0 {
		line := fmt.Sprintf("%s capabilities^{}\x00%s\n", ZeroOid, caps)
		if err := pw.WritePktString(line); err != nil {
			return nil, err
		}
		if err := pw.WriteFlush(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	// 非空仓库：upload-pack 首行发 HEAD（带 caps），再发实际 refs；
	// receive-pack 不发 HEAD（与 cgit 一致），首行实际 ref 带 caps。
	i := 0
	if service == "git-upload-pack" && head != nil {
		line := fmt.Sprintf("%s %s\x00%s\n", head.Oid, head.Name, caps)
		if err := pw.WritePktString(line); err != nil {
			return nil, err
		}
		i++
	}
	for _, r := range actualRefs {
		var line string
		if i == 0 {
			line = fmt.Sprintf("%s %s\x00%s\n", r.Oid, r.Name, caps)
		} else {
			line = fmt.Sprintf("%s %s\n", r.Oid, r.Name)
		}
		if err := pw.WritePktString(line); err != nil {
			return nil, err
		}
		i++
	}
	if err := pw.WriteFlush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ServeUploadPack 处理 upload-pack 请求（clone/fetch）。
// in 从 wants 开始（ref advertisement 由 AdvertiseRefs 单独生成）。
// 基本模式 v0（不广告 multi_ack_detailed）：客户端分批发 wants/haves/done。
//   - wants 阶段：want 行序列以 flush 结束
//   - haves 阶段：have 行分批，每批以 flush 结束；服务端在每个 flush 处回 NAK
//     （不带 flush pkt，避免客户端 get_ack 误读 flush 报 die）
//   - done 结束 negotiation，服务端发 NAK + PACK + flush
//
// HTTP stateless_rpc：每个 POST 是一次 ServeUploadPack 调用，have 批的 flush
//
//	后请求体结束（EOF），此时仅已发 NAK 响应，不发 PACK 直接 return；
//	最终含 done 的 POST 才发 NAK + PACK + flush。
//
// SSH 流式：一次调用处理整轮 negotiation，have flush 后 continue 继续读。
// NAK 数与 fetch-pack 客户端 get_ack 次数自洽（对齐 fetch-pack.c:619-628）。
// NAK 作为普通 pkt-line 写到 pw（sideband 模式下 NAK 不走 ch1，与 cgit 一致）。
// PACK 数据：sideband 模式走 ch1，否则直接写 out。
func ServeUploadPack(repoRoot string, in io.Reader, out io.Writer) error {
	pr := NewPktReader(in)
	pw := NewPktWriter(out)
	tStart := time.Now()
	tAfterNegotiation := tStart

	// 详细日志：建立 oid→refname 映射，供 want oid 反查来源 ref（标准 want 行不含 refname）。
	refOf := map[Oid]string{}
	if currentLogLevel() >= LogDetail {
		if refs, err := NewRefStore(repoRoot).List(); err == nil {
			for _, r := range refs {
				if _, ok := refOf[r.Oid]; !ok {
					refOf[r.Oid] = r.Name
				}
			}
		}
	}
	refName := func(oid Oid) string {
		if n, ok := refOf[oid]; ok {
			return n
		}
		return "-"
	}

	// 1. 读首行 want（格式 "<oid> <refname>\0<caps>" 或 "want <oid> <caps>"）
	first, isFlush, err := pr.ReadPkt()
	if err != nil {
		return fmt.Errorf("upload-pack: read first want: %w", err)
	}
	if isFlush {
		return fmt.Errorf("upload-pack: unexpected flush as first frame")
	}
	firstOid, clientCaps, ok := parseWantLine(string(first))
	if !ok {
		return fmt.Errorf("upload-pack: no want oid in first line %q", first)
	}
	wantOids := []Oid{firstOid}
	if currentLogLevel() >= LogDetail {
		debugf("upload-pack want", "oid", string(firstOid), "ref", refName(firstOid))
	}

	// 2. 继续读 want 行直到 flush
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			return fmt.Errorf("upload-pack: read wants: %w", err)
		}
		if isFlush {
			break
		}
		oid, _, ok := parseWantLine(string(payload))
		if ok {
			wantOids = append(wantOids, oid)
			if currentLogLevel() >= LogDetail {
				debugf("upload-pack want", "oid", string(oid), "ref", refName(oid))
			}
		}
	}

	// 3. 读 have 行直到 done（或 EOF）
	// 基本模式 v0：have 阶段每个 flush 处回 NAK 响应客户端这批 have（对齐
	// upload-pack.c:595-606）。HTTP stateless_rpc 下 flush 后请求体结束（EOF），
	// 前面已发 NAK，直接 return 不发 PACK；SSH 流式下 continue 继续读下一批。
	var haveOids []Oid
	sawDone := false
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err == io.EOF {
			// HTTP 中间请求（have flush 后请求体结束，无 done）：前面 flush 已发 NAK
			break
		}
		if err != nil {
			return fmt.Errorf("upload-pack: read have/done: %w", err)
		}
		if isFlush {
			// 基本模式：flush 时回 NAK（不带 flush pkt）。
			// 客户端 get_ack 读 NAK 返回 0 退出 do-while，flushes--；
			// 若发 flush 会被 get_ack 误读为 "got a flush packet" 报 die。
			if err := pw.WritePktString("NAK\n"); err != nil {
				return fmt.Errorf("upload-pack: write NAK (have flush): %w", err)
			}
			continue
		}
		line := string(payload)
		if strings.HasPrefix(line, "done") {
			sawDone = true
			break
		}
		if strings.HasPrefix(line, "have ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				ho := Oid(fields[1])
				haveOids = append(haveOids, ho)
				if currentLogLevel() >= LogDetail {
					debugf("upload-pack have", "oid", string(ho))
				}
			}
		}
	}

	// HTTP stateless_rpc 中间请求（have flush 后 EOF，无 done）：
	// 前面 flush 已发 NAK 响应，本次请求结束，不发 PACK。
	if !sawDone {
		return nil
	}
	tAfterNegotiation = time.Now()

	// 4. done 后发 NAK 终结 negotiation（基本模式 v0，无 multi_ack）。
	// NAK 作为普通 pkt-line 写到 pw，不走 sideband ch1（与 cgit 一致）。
	if err := pw.WritePktString("NAK\n"); err != nil {
		return fmt.Errorf("upload-pack: write NAK: %w", err)
	}

	// 5. 只读对象头部（Stat）遍历发送集合：内容不入内存，
	//    内存占用与仓库体积无关（blob 无需解压即可获知 type/size）。
	store := NewObjectStore(repoRoot)
	metas, err := WalkReachable(store, wantOids, haveOids, nil)
	if err != nil {
		return fmt.Errorf("upload-pack: walk reachable: %w", err)
	}
	tAfterReach := time.Now()
	if currentLogLevel() >= LogDetail {
		for _, m := range metas {
			debugf("upload-pack object", "oid", string(m.Oid), "type", string(m.Type), "size", m.Size)
		}
	}

	// 6. want 全部已被 have 覆盖 → 仅发 NAK + flush，不发 PACK
	if len(metas) == 0 {
		slog.Info("upload-pack up-to-date", "wants", len(wantOids), "haves", len(haveOids))
		if err := pw.WriteFlush(); err != nil {
			return fmt.Errorf("upload-pack: flush (no pack): %w", err)
		}
		return nil
	}

	// 6+7. 单遍编码：非 blob 按 BFS 顺序先写；blob 按 size 降序遍历两两配对
	//      （base=大者），保证 OFS_DELTA 的 base 先于 delta 出现。每个对象只读一次，
	//      内存峰值 ≈ 同时持有的一对 blob + 已算出的 delta。
	useSideband := strings.Contains(clientCaps, "side-band-64k")
	var packSink io.Writer
	if useSideband {
		packSink = NewSidebandWriter(pw, SidebandPack)
	} else {
		packSink = out
	}
	if err := encodePack(store, metas, packSink); err != nil {
		return fmt.Errorf("upload-pack: encode pack: %w", err)
	}
	tAfterEncode := time.Now()

	// 8. flush 结束
	if err := pw.WriteFlush(); err != nil {
		return fmt.Errorf("upload-pack: flush: %w", err)
	}
	slog.Info("upload-pack done",
		"wants", len(wantOids), "haves", len(haveOids), "objects", len(metas),
		"negotiateMs", tAfterNegotiation.Sub(tStart).Milliseconds(),
		"reachMs", tAfterReach.Sub(tAfterNegotiation).Milliseconds(),
		"encodeMs", tAfterEncode.Sub(tAfterReach).Milliseconds(),
		"totalMs", time.Since(tStart).Milliseconds())
	return nil
}

// ServeReceivePack 处理 receive-pack 请求（push）。
// in: ref updates（pkt-line）+ flush + packfile 二进制。
// out: report-status（sideband ch1 或直接 pkt-line）。
// push 仅 CAS（old-oid 校验），不做可达性检查，逐对象 SHA1 校验。
func ServeReceivePack(repoRoot string, in io.Reader, out io.Writer) error {
	// bufio 包装：既能给 PktReader 精确读帧，又可在 ref 更新结束后 peek 是否还有 pack。
	br, ok := in.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(in)
	}
	pr := NewPktReader(br)
	pw := NewPktWriter(out)

	// 1. 读首行 ref update + caps
	first, isFlush, err := pr.ReadPkt()
	if err != nil {
		return fmt.Errorf("receive-pack: read first update: %w", err)
	}
	// 首帧为 flush：空命令列表请求（body 仅含 flush-pkt，无 ref 更新、无 packfile）。
	// 与 cgit 一致，返回空 report-status（unpack ok + flush-pkt）而非错误。
	if isFlush {
		slog.Info("receive-pack empty command list")
		if err := pw.WritePktString("unpack ok\n"); err != nil {
			return fmt.Errorf("receive-pack: write unpack status: %w", err)
		}
		return pw.WriteFlush()
	}
	firstUpdate, clientCaps, ok := parseUpdateLine(string(first))
	if !ok {
		return fmt.Errorf("receive-pack: bad first line %q", first)
	}
	updates := []RefUpdate{firstUpdate}

	// 2. 继续读 ref update 行直到 flush
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			return fmt.Errorf("receive-pack: read updates: %w", err)
		}
		if isFlush {
			break
		}
		u, _, ok := parseUpdateLine(string(payload))
		if ok {
			updates = append(updates, u)
		}
	}

	// 3+4. 流式读 packfile 并逐对象落盘：对象内容不入内存（峰值 ≈ 单个最大对象 +
	//      其 delta base），SHA1 在写入前逐对象重算校验。total 上限由 maxReceivePackBytes 兜底。
	store := NewObjectStore(repoRoot)
	received := 0
	packErr := error(nil)
	if _, err := br.Peek(1); err == nil {
		dec := NewPackDecoder(io.LimitReader(br, maxReceivePackBytes.Load()))
		received, packErr = dec.DecodeTo(store)
	}
	if packErr != nil {
		// pack 不可用（超限/损坏）：不更新任何 ref，但必须回 report-status 让客户端
		// 立刻得到明确拒绝，而不是等待连接关闭。
		slog.Error("receive-pack pack rejected", "error", packErr)
		rejected := make([]RefUpdateResult, 0, len(updates))
		for _, u := range updates {
			rejected = append(rejected, RefUpdateResult{Name: u.Name})
		}
		return writeReportStatus(out, clientCaps, rejected, packErr)
	}
	if received > 0 {
		slog.Info("receive-pack received objects", "objects", received)
	}

	// 5. RefStore.Update（per-ref 原子 CAS）
	rs := NewRefStore(repoRoot)
	results, err := rs.Update(updates)
	if err != nil {
		return fmt.Errorf("receive-pack: update refs: %w", err)
	}

	// 5.5 日志：记录每个 ref 更新结果，标记 force-push（非快进推送）
	for i, u := range updates {
		if i < len(results) && results[i].Ok {
			tag := ""
			if u.OldOid != ZeroOid && u.NewOid != ZeroOid {
				if !isFastForward(store, u.OldOid, u.NewOid) {
					tag = " [force-push]"
				}
			}
			slog.Info("receive-pack ref updated", "ref", u.Name, "old", oidShort(u.OldOid), "new", oidShort(u.NewOid), "forcePush", tag != "")
		} else if i < len(results) {
			slog.Warn("receive-pack ref rejected", "ref", u.Name, "reason", results[i].Reason)
		}
	}

	// 6. 回 report-status
	return writeReportStatus(out, clientCaps, results, nil)
}

// writeReportStatus 输出 report-status（unpack 行 + 每 ref 结果 + flush）。
// sideband 客户端下 pkt-line 经 ch1 封装，并在最后额外发一个外层 flush。
// unpackErr 非 nil 时所有 ref 记为 ng（如 pack 超限/损坏）。
func writeReportStatus(out io.Writer, clientCaps string, results []RefUpdateResult, unpackErr error) error {
	pw := NewPktWriter(out)
	useSideband := strings.Contains(clientCaps, "side-band-64k")
	var statusPw *PktWriter
	if useSideband {
		// report-status 的 pkt-line 帧经 sideband ch1 封装；
		// 接收方先重组 ch1 数据流再解析 pkt-line，故 PktWriter 的两次写（header+payload）
		// 各成一帧但重组后仍为完整 pkt-line。
		statusPw = NewPktWriter(NewSidebandWriter(pw, SidebandPack))
	} else {
		statusPw = pw
	}
	unpackLine := "unpack ok\n"
	if unpackErr != nil {
		unpackLine = fmt.Sprintf("unpack error: %v\n", unpackErr)
	}
	if err := statusPw.WritePktString(unpackLine); err != nil {
		return fmt.Errorf("receive-pack: write unpack status: %w", err)
	}
	for _, r := range results {
		var line string
		switch {
		case unpackErr != nil:
			line = fmt.Sprintf("ng %s unpacker error\n", r.Name)
		case r.Ok:
			line = fmt.Sprintf("ok %s\n", r.Name)
		default:
			line = fmt.Sprintf("ng %s %s\n", r.Name, r.Reason)
		}
		if err := statusPw.WritePktString(line); err != nil {
			return fmt.Errorf("receive-pack: write ref status: %w", err)
		}
	}
	// report-status 序列结束 flush-pkt：客户端按 "unpack-status + command-status-list + flush-pkt"
	// 解析，缺此 flush 会报「远端意外挂断」。sideband 下此 flush 经 ch1 封装重组。
	if err := statusPw.WriteFlush(); err != nil {
		return fmt.Errorf("receive-pack: report-status flush: %w", err)
	}
	// sideband 模式需额外结束 sideband 流（report-status flush 仅结束 ch1 数据，不结束外层 pkt-line）
	if useSideband {
		if err := pw.WriteFlush(); err != nil {
			return fmt.Errorf("receive-pack: sideband flush: %w", err)
		}
	}
	return nil
}

// parseWantLine 解析 want 行，返回 oid + caps。
// 支持两种首行格式：
//
//	"<oid> <refname>\0<caps>"   （带 NUL + caps）
//	"want <oid> <caps>"          （标准 v0，caps 空格分隔）
//
// 以及后续行 "want <oid>"。
func parseWantLine(line string) (oid Oid, caps string, ok bool) {
	line = strings.TrimRight(line, "\n")
	var main, capPart string
	if i := strings.IndexByte(line, 0); i >= 0 {
		main = line[:i]
		capPart = line[i+1:]
	} else {
		main = line
	}
	fields := strings.Fields(main)
	if len(fields) >= 2 && fields[0] == "want" {
		oid = Oid(fields[1])
		if capPart == "" && len(fields) > 2 {
			capPart = strings.Join(fields[2:], " ")
		}
		ok = true
	} else if len(fields) >= 1 {
		oid = Oid(fields[0])
		ok = true
	}
	caps = capPart
	return
}

// parseUpdateLine 解析 receive-pack 的 ref update 行。
// 格式："<old> <new> <refname>\0<caps>"（首行）或 "<old> <new> <refname>"（后续）。
func parseUpdateLine(line string) (u RefUpdate, caps string, ok bool) {
	line = strings.TrimRight(line, "\n")
	var main, capPart string
	if i := strings.IndexByte(line, 0); i >= 0 {
		main = line[:i]
		capPart = line[i+1:]
	} else {
		main = line
	}
	fields := strings.Fields(main)
	if len(fields) >= 3 {
		u = RefUpdate{
			OldOid: Oid(fields[0]),
			NewOid: Oid(fields[1]),
			Name:   fields[2],
		}
		ok = true
	}
	caps = capPart
	return
}

func oidShort(o Oid) string {
	if len(o) >= 7 {
		return string(o[:7])
	}
	return string(o)
}

// isFastForward 检查 ancestor 是否是 descendant 的祖先（快进推送）。
// 从 descendant 沿 parent 链 BFS，若能到达 ancestor 则为快进。
// 任一对象读取失败视为非快进（保守策略，不影响推送本身）。
func isFastForward(store ObjectStore, ancestor, descendant Oid) bool {
	visited := make(map[Oid]bool)
	queue := []Oid{descendant}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == ancestor {
			return true
		}
		if visited[oid] {
			continue
		}
		visited[oid] = true
		obj, err := store.Read(oid)
		if err != nil {
			continue
		}
		if obj.Type != ObjCommit {
			continue
		}
		c, err := ParseCommit(obj.Content)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if !p.IsZero() {
				queue = append(queue, p)
			}
		}
	}
	return false
}

// encodePack 单遍把 metas 中的对象编码为 pack 写入 w，返回写入对象数。
//
// 顺序：非 blob 保持 BFS 顺序；blob 按 size 降序两两配对（base=较大者）。
// base 总在 delta 之前写出 → OFS_DELTA 合法。每对只读取一次对象内容，
// 因此每个对象仅解压一次，内存峰值 ≈ 一对 blob + delta 字节。
func encodePack(store ObjectStore, metas []ObjectMeta, w io.Writer) error {
	others := make([]ObjectMeta, 0, len(metas))
	blobs := make([]ObjectMeta, 0, len(metas))
	for _, m := range metas {
		if m.Type == ObjBlob {
			blobs = append(blobs, m)
		} else {
			others = append(others, m)
		}
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Size > blobs[j].Size })

	enc := NewPackEncoder(w)
	if err := enc.WriteHeader(len(metas)); err != nil {
		return fmt.Errorf("pack header: %w", err)
	}

	writeFull := func(oid Oid) error {
		obj, err := store.Read(oid)
		if err != nil {
			return fmt.Errorf("read %s: %w", oid, err)
		}
		if err := enc.WriteObject(obj); err != nil {
			return fmt.Errorf("pack obj %s: %w", oid, err)
		}
		return nil
	}

	for _, m := range others {
		if err := writeFull(m.Oid); err != nil {
			return err
		}
	}

	for i := 0; i < len(blobs); i += 2 {
		base := blobs[i]
		if i+1 >= len(blobs) {
			if err := writeFull(base.Oid); err != nil {
				return err
			}
			break
		}
		tgt := blobs[i+1]

		baseObj, err := store.Read(base.Oid)
		if err != nil {
			return fmt.Errorf("read base %s: %w", base.Oid, err)
		}
		tgtObj, err := store.Read(tgt.Oid)
		if err != nil {
			return fmt.Errorf("read target %s: %w", tgt.Oid, err)
		}

		hi, lo := base.Size, tgt.Size
		if hi < lo {
			hi, lo = lo, hi
		}
		useDelta := false
		var delta []byte
		if hi <= 2*lo && deltaPrecheck(baseObj.Content, tgtObj.Content) {
			d, err := EncodeDelta(baseObj.Content, tgtObj.Content)
			if err != nil {
				return fmt.Errorf("encode delta base=%s tgt=%s: %w", base.Oid, tgt.Oid, err)
			}
			if len(d)*2 < tgt.Size { // 负收益回退
				useDelta = true
				delta = d
			} else if currentLogLevel() >= LogDetail {
				debugf("upload-pack delta fallback (negative)", "base", string(base.Oid), "target", string(tgt.Oid), "deltaLen", len(d), "tgtSize", tgt.Size)
			}
		} else if currentLogLevel() >= LogDetail {
			debugf("upload-pack delta skip", "base", string(base.Oid), "target", string(tgt.Oid), "hi", hi, "lo", lo)
		}

		if err := enc.WriteObject(baseObj); err != nil {
			return fmt.Errorf("pack obj %s: %w", base.Oid, err)
		}
		if useDelta {
			if err := enc.WriteOfsDelta(base.Oid, delta); err != nil {
				return fmt.Errorf("pack delta %s: %w", tgt.Oid, err)
			}
			if currentLogLevel() >= LogDetail {
				debugf("upload-pack delta", "base", string(base.Oid), "target", string(tgt.Oid), "baseSize", base.Size, "tgtSize", tgt.Size, "deltaLen", len(delta))
			}
		} else {
			if err := enc.WriteObject(tgtObj); err != nil {
				return fmt.Errorf("pack obj %s: %w", tgt.Oid, err)
			}
		}
	}

	if err := enc.WriteTrailer(); err != nil {
		return fmt.Errorf("pack trailer: %w", err)
	}
	return nil
}
