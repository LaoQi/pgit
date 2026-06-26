package git

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

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
// 基本模式 v0（不广告 multi_ack_detailed）：客户端单轮发 wants+haves+done，
// 服务端 done 后发 NAK + PACK + flush。无交互式 ACK。
// NAK 作为普通 pkt-line 写到 pw（sideband 模式下 NAK 不走 ch1，与 cgit 一致）。
// PACK 数据：sideband 模式走 ch1，否则直接写 out。
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
	}

	// 4. 发 NAK 终结 negotiation（基本模式 v0，无 multi_ack）。
	// NAK 作为普通 pkt-line 写到 pw，不走 sideband ch1（与 cgit 一致）。
	if err := pw.WritePktString("NAK\n"); err != nil {
		return fmt.Errorf("upload-pack: write NAK: %w", err)
	}

	// 5. CollectReachable
	store := &LooseStore{Root: filepath.Join(repoRoot, "objects")}
	objs, err := CollectReachable(store, wantOids)
	if err != nil {
		return fmt.Errorf("upload-pack: collect reachable: %w", err)
	}

	// 6. 规划 delta 配对（仅 blob，单层，OFS_DELTA）
	entries, err := planPackEntries(objs)
	if err != nil {
		return fmt.Errorf("upload-pack: plan deltas: %w", err)
	}

	// 7. 编码 pack（可能走 sideband）
	useSideband := strings.Contains(clientCaps, "side-band-64k")
	var packSink io.Writer
	if useSideband {
		packSink = NewSidebandWriter(pw, SidebandPack)
	} else {
		packSink = out
	}
	enc := NewPackEncoder(packSink)
	if err := enc.WriteHeader(len(entries)); err != nil {
		return fmt.Errorf("upload-pack: pack header: %w", err)
	}
	for _, e := range entries {
		if !e.isDelta {
			if err := enc.WriteObject(e.obj); err != nil {
				return fmt.Errorf("upload-pack: pack obj %s: %w", e.obj.Oid(), err)
			}
		}
	}
	for _, e := range entries {
		if e.isDelta {
			if err := enc.WriteOfsDelta(e.baseOid, e.delta); err != nil {
				return fmt.Errorf("upload-pack: pack delta %s: %w", e.obj.Oid(), err)
			}
		}
	}
	if err := enc.WriteTrailer(); err != nil {
		return fmt.Errorf("upload-pack: pack trailer: %w", err)
	}

	// 8. flush 结束
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
	// 首帧为 flush：空命令列表请求（body 仅含 flush-pkt，无 ref 更新、无 packfile）。
	// 与 cgit 一致，返回空 report-status（unpack ok + flush-pkt）而非错误。
	if isFlush {
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

	// 3. 读 packfile（剩余 in 全部）
	remaining, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("receive-pack: read pack: %w", err)
	}
	var objs []*RawObject
	store := &LooseStore{Root: filepath.Join(repoRoot, "objects")}
	if len(remaining) > 0 {
		dec := NewPackDecoder(bytes.NewReader(remaining), store)
		objs, err = dec.Decode()
		if err != nil {
			return fmt.Errorf("receive-pack: decode pack: %w", err)
		}
	}

	// 4. 逐对象 SHA1 重算校验 + LooseStore.Write
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

// packEntry 描述一个对象在 pack 中的写入方式
type packEntry struct {
	obj     *RawObject
	isDelta bool
	baseOid Oid    // isDelta=true：base 的 oid（须已作为 full 写入）
	delta   []byte // isDelta=true：EncodeDelta 输出
}

// planPackEntries 规划 pack 写入计划：仅 blob 做 delta，单层（base 必 full），OFS_DELTA。
// 策略：blob 按 size 降序相邻两两配对（base=大者 full，target=小者 delta）；
//   - size 比值过滤：max/min > 2 不配对（差异过大 delta 收益低）；
//   - 负收益回退：deltaLen*2 >= tgt.Size 时 target 退化为 full；
//   - 非 blob 全 full；落单 blob 全 full。
// entries 保持 objs 原 BFS 顺序，仅标记 isDelta；ServeUploadPack 两段写入（full 先于 delta）。
func planPackEntries(objs []*RawObject) ([]packEntry, error) {
	entries := make([]packEntry, len(objs))
	blobIdx := map[*RawObject]int{}
	var blobs []*RawObject
	for i, o := range objs {
		entries[i].obj = o
		if o.Type == ObjBlob {
			blobs = append(blobs, o)
			blobIdx[o] = i
		}
	}
	// blob 按 size 降序拷贝（不影响 entries 原序）
	sorted := make([]*RawObject, len(blobs))
	copy(sorted, blobs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Size > sorted[j].Size })
	// 相邻两两配对：i 为 base（更大），i+1 为 target
	for i := 0; i+1 < len(sorted); i += 2 {
		base, tgt := sorted[i], sorted[i+1]
		// size 比值过滤：max/min > 2 跳过
		hi, lo := base.Size, tgt.Size
		if hi < lo {
			hi, lo = lo, hi
		}
		if hi > 2*lo {
			continue
		}
		delta, err := EncodeDelta(base.Content, tgt.Content)
		if err != nil {
			return nil, fmt.Errorf("encode delta base=%s tgt=%s: %w", base.Oid(), tgt.Oid(), err)
		}
		// 负收益回退：delta 字节数 >= target 原始字节数一半 → 退化为 full
		if len(delta)*2 >= tgt.Size {
			continue
		}
		// 标记 target 为 delta（base 保持 full）
		j := blobIdx[tgt]
		entries[j].isDelta = true
		entries[j].baseOid = base.Oid()
		entries[j].delta = delta
	}
	return entries, nil
}
