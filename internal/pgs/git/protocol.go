package git

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// upload-pack v0 capabilities
const uploadPackCaps = "multi_ack_detailed thin-pack side-band-64k ofs-delta no-progress include-tag"

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

	var buf bytes.Buffer
	pw := NewPktWriter(&buf)

	if len(refs) == 0 {
		// 空仓库：<ZeroOid> capabilities^{}\n + flush
		line := fmt.Sprintf("%s capabilities^{}\n", ZeroOid)
		if err := pw.WritePktString(line); err != nil {
			return nil, err
		}
		if err := pw.WriteFlush(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	for i, r := range refs {
		var line string
		if i == 0 {
			// 第一行带 capabilities（NUL 分隔）
			line = fmt.Sprintf("%s %s\x00%s\n", r.Oid, r.Name, caps)
		} else {
			line = fmt.Sprintf("%s %s\n", r.Oid, r.Name)
		}
		if err := pw.WritePktString(line); err != nil {
			return nil, err
		}
	}
	if err := pw.WriteFlush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ServeUploadPack 处理 upload-pack 请求（clone/fetch）。
// in 从 wants 开始（ref advertisement 由 AdvertiseRefs 单独生成）。
// pack 走 sideband-64k ch1（若客户端 caps 含 side-band-64k），否则直接写 out。
func ServeUploadPack(repoRoot string, in io.Reader, out io.Writer) error {
	pr := NewPktReader(in)
	pw := NewPktWriter(out)

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
		}
	}

	// 3. 读 have 行（忽略）直到 done（或 EOF）
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("upload-pack: read have/done: %w", err)
		}
		if isFlush {
			continue
		}
		line := string(payload)
		if strings.HasPrefix(line, "done") {
			break
		}
		// have 行或其他，忽略
	}

	// 4. 客户端发 done 后，服务端先发 NAK（multi_ack_detailed / stateless 协议要求）
	if err := pw.WritePktString("NAK\n"); err != nil {
		return fmt.Errorf("upload-pack: write NAK: %w", err)
	}

	// 5. CollectReachable
	store := &LooseStore{Root: filepath.Join(repoRoot, "objects")}
	objs, err := CollectReachable(store, wantOids)
	if err != nil {
		return fmt.Errorf("upload-pack: collect reachable: %w", err)
	}

	// 6. 编码 pack（可能走 sideband）
	useSideband := strings.Contains(clientCaps, "side-band-64k")
	var packSink io.Writer
	if useSideband {
		packSink = NewSidebandWriter(pw, SidebandPack)
	} else {
		packSink = out
	}
	enc := NewPackEncoder(packSink)
	if err := enc.WriteHeader(len(objs)); err != nil {
		return fmt.Errorf("upload-pack: pack header: %w", err)
	}
	for _, o := range objs {
		if err := enc.WriteObject(o); err != nil {
			return fmt.Errorf("upload-pack: pack obj %s: %w", o.Oid(), err)
		}
	}
	if err := enc.WriteTrailer(); err != nil {
		return fmt.Errorf("upload-pack: pack trailer: %w", err)
	}

	// 6. flush 结束
	if err := pw.WriteFlush(); err != nil {
		return fmt.Errorf("upload-pack: flush: %w", err)
	}
	return nil
}

// ServeReceivePack 处理 receive-pack 请求（push）。
// in: ref updates（pkt-line）+ flush + packfile 二进制。
// out: report-status（sideband ch1 或直接 pkt-line）。
// push 仅 CAS（old-oid 校验），不做可达性检查，逐对象 SHA1 校验。
func ServeReceivePack(repoRoot string, in io.Reader, out io.Writer) error {
	pr := NewPktReader(in)
	pw := NewPktWriter(out)

	// 1. 读首行 ref update + caps
	first, isFlush, err := pr.ReadPkt()
	if err != nil {
		return fmt.Errorf("receive-pack: read first update: %w", err)
	}
	if isFlush {
		return fmt.Errorf("receive-pack: unexpected flush as first frame")
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

	// 3. 读 packfile（剩余 in 全部）
	remaining, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("receive-pack: read pack: %w", err)
	}
	var objs []*RawObject
	if len(remaining) > 0 {
		dec := NewPackDecoder(bytes.NewReader(remaining))
		objs, err = dec.Decode()
		if err != nil {
			return fmt.Errorf("receive-pack: decode pack: %w", err)
		}
	}

	// 4. 逐对象 SHA1 重算校验 + LooseStore.Write
	store := &LooseStore{Root: filepath.Join(repoRoot, "objects")}
	for _, obj := range objs {
		oid := obj.Oid()
		if !oid.Valid() {
			return fmt.Errorf("receive-pack: invalid oid for %s size %d", obj.Type, obj.Size)
		}
		if _, err := store.Write(obj); err != nil {
			return fmt.Errorf("receive-pack: write %s: %w", oid, err)
		}
	}

	// 5. RefStore.Update（per-ref 原子 CAS）
	rs := NewRefStore(repoRoot)
	results, err := rs.Update(updates)
	if err != nil {
		return fmt.Errorf("receive-pack: update refs: %w", err)
	}

	// 6. 回 report-status
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
	if err := statusPw.WritePktString("unpack ok\n"); err != nil {
		return fmt.Errorf("receive-pack: write unpack status: %w", err)
	}
	for _, r := range results {
		var line string
		if r.Ok {
			line = fmt.Sprintf("ok %s\n", r.Name)
		} else {
			line = fmt.Sprintf("ng %s %s\n", r.Name, r.Reason)
		}
		if err := statusPw.WritePktString(line); err != nil {
			return fmt.Errorf("receive-pack: write ref status: %w", err)
		}
	}
	// flush（始终直接写 pw，结束 sideband 流）
	if err := pw.WriteFlush(); err != nil {
		return fmt.Errorf("receive-pack: flush: %w", err)
	}
	return nil
}

// parseWantLine 解析 want 行，返回 oid + caps。
// 支持两种首行格式：
//   "<oid> <refname>\0<caps>"   （带 NUL + caps）
//   "want <oid> <caps>"          （标准 v0，caps 空格分隔）
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
