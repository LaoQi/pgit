package git

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeRepoWithCommit 构造一个含 blob+tree+commit + refs/heads/master + HEAD 的仓库，
// 返回仓库根目录与 commit oid。复用 reach_test.go 的 makeBlob/makeTree/makeCommit/writeAll
// 与 refs_test.go 的 writeRefFile。
func makeRepoWithCommit(t *testing.T) (string, Oid) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pgit-proto-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	store := &LooseStore{Root: filepath.Join(dir, "objects")}
	blob := makeBlob("hello pgit\n")
	tree := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob.Oid()},
	})
	commit := makeCommit(tree.Oid(), nil, "initial commit\n")
	writeAll(t, store, blob, tree, commit)

	rs := NewRefStore(dir)
	if _, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: ZeroOid, NewOid: commit.Oid()},
	}); err != nil {
		t.Fatalf("update refs: %v", err)
	}
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/master\n")
	return dir, commit.Oid()
}

// TestAdvertiseRefsWithRefs: 构造小仓库，验证 advertisement 含 oid+refname+caps+HEAD
func TestAdvertiseRefsWithRefs(t *testing.T) {
	dir, commitOid := makeRepoWithCommit(t)

	adv, err := AdvertiseRefs(dir, "git-upload-pack")
	if err != nil {
		t.Fatalf("AdvertiseRefs: %v", err)
	}

	pr := NewPktReader(bytes.NewReader(adv))
	var lines []string
	var firstCaps string
	i := 0
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if isFlush {
			break
		}
		line := string(payload)
		if i == 0 {
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				firstCaps = line[nul+1:]
				line = line[:nul]
			}
		}
		lines = append(lines, strings.TrimRight(line, "\n"))
		i++
	}

	// 第一行 caps 含 side-band-64k 与 ofs-delta；不应广告 multi_ack（基本模式 v0 多轮
	// negotiation 已实现：have flush 回 NAK；不实现 multi_ack/multi_ack_detailed 交互式 ACK）
	if !strings.Contains(firstCaps, "side-band-64k") {
		t.Errorf("first caps missing side-band-64k: %q", firstCaps)
	}
	if !strings.Contains(firstCaps, "ofs-delta") {
		t.Errorf("first caps missing ofs-delta: %q", firstCaps)
	}
	if strings.Contains(firstCaps, "multi_ack") {
		t.Errorf("caps must not advertise multi_ack (no multi-round negotiation): %q", firstCaps)
	}

	// 验证含 HEAD 行与 refs/heads/master 行，oid 正确
	foundHead, foundMaster := false, false
	for _, l := range lines {
		sp := strings.IndexByte(l, ' ')
		if sp < 0 {
			continue
		}
		oid, name := l[:sp], l[sp+1:]
		switch name {
		case "HEAD":
			foundHead = true
			if oid != string(commitOid) {
				t.Errorf("HEAD oid = %s, want %s", oid, commitOid)
			}
		case "refs/heads/master":
			foundMaster = true
			if oid != string(commitOid) {
				t.Errorf("master oid = %s, want %s", oid, commitOid)
			}
		}
	}
	if !foundHead {
		t.Errorf("no HEAD line in advertisement: %v", lines)
	}
	if !foundMaster {
		t.Errorf("no refs/heads/master line: %v", lines)
	}
}

// TestAdvertiseRefsReceivePackCaps: receive-pack advertisement caps
func TestAdvertiseRefsReceivePackCaps(t *testing.T) {
	dir, _ := makeRepoWithCommit(t)

	adv, err := AdvertiseRefs(dir, "git-receive-pack")
	if err != nil {
		t.Fatalf("AdvertiseRefs: %v", err)
	}

	pr := NewPktReader(bytes.NewReader(adv))
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if isFlush {
		t.Fatal("expected data frame, got flush")
	}
	line := string(payload)
	nul := strings.IndexByte(line, 0)
	if nul < 0 {
		t.Fatalf("first line missing NUL+caps: %q", line)
	}
	caps := line[nul+1:]
	if !strings.Contains(caps, "report-status") {
		t.Errorf("receive-pack caps missing report-status: %q", caps)
	}
	if !strings.Contains(caps, "delete-refs") {
		t.Errorf("receive-pack caps missing delete-refs: %q", caps)
	}
	if !strings.Contains(caps, "side-band-64k") {
		t.Errorf("receive-pack caps missing side-band-64k: %q", caps)
	}
}

// TestAdvertiseRefsEmptyRepo: 空仓库 → ZeroOid + "capabilities^{}"
func TestAdvertiseRefsEmptyRepo(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-empty-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	adv, err := AdvertiseRefs(dir, "git-upload-pack")
	if err != nil {
		t.Fatalf("AdvertiseRefs: %v", err)
	}

	pr := NewPktReader(bytes.NewReader(adv))
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	if isFlush {
		t.Fatal("expected data frame, got flush")
	}
	line := string(payload)
	if !strings.Contains(line, string(ZeroOid)) {
		t.Errorf("empty adv missing ZeroOid: %q", line)
	}
	if !strings.Contains(line, "capabilities^{}") {
		t.Errorf("empty adv missing capabilities^{}: %q", line)
	}
	// 接下来应是 flush
	_, isFlush, err = pr.ReadPkt()
	if err != nil || !isFlush {
		t.Fatalf("expected flush after empty adv: err=%v isFlush=%v", err, isFlush)
	}
}

// TestServeInfoRefs: 完整 smart-http 响应 = "# service=" 帧 + flush + ref advertisement
func TestServeInfoRefs(t *testing.T) {
	dir, commitOid := makeRepoWithCommit(t)

	out, err := ServeInfoRefs(dir, "git-upload-pack")
	if err != nil {
		t.Fatalf("ServeInfoRefs: %v", err)
	}

	pr := NewPktReader(bytes.NewReader(out))
	// 第一帧：# service=git-upload-pack\n
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read service: %v", err)
	}
	if isFlush {
		t.Fatal("expected service frame, got flush")
	}
	if !strings.Contains(string(payload), "# service=git-upload-pack") {
		t.Errorf("service frame = %q, want # service=git-upload-pack", payload)
	}
	// 接下来 flush
	_, isFlush, err = pr.ReadPkt()
	if err != nil || !isFlush {
		t.Fatalf("expected flush after service: err=%v isFlush=%v", err, isFlush)
	}
	// 之后是 ref advertisement，第一行含 commit oid + caps（NUL）
	payload, isFlush, err = pr.ReadPkt()
	if err != nil {
		t.Fatalf("read ref: %v", err)
	}
	if isFlush {
		t.Fatal("expected ref frame, got flush")
	}
	if !strings.Contains(string(payload), string(commitOid)) {
		t.Errorf("first ref frame missing commit oid %s: %q", commitOid, payload)
	}
	if !strings.Contains(string(payload), "side-band-64k") {
		t.Errorf("first ref frame missing caps: %q", payload)
	}
}

// TestServeUploadPackRoundTrip: wants → ServeUploadPack → NAK + sideband pack → 解码 → 对象一致
func TestServeUploadPackRoundTrip(t *testing.T) {
	dir, commitOid := makeRepoWithCommit(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	if err := inw.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commitOid)); err != nil {
		t.Fatal(err)
	}
	if err := inw.WriteFlush(); err != nil {
		t.Fatal(err)
	}
	if err := inw.WritePktString("done\n"); err != nil {
		t.Fatal(err)
	}

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	// sideband 模式：首帧是 NAK pkt-line（不走 ch1），后续帧是 ch1 sideband
	pr := NewPktReader(&outBuf)
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read NAK: %v", err)
	}
	if isFlush {
		t.Fatal("expected NAK frame, got flush")
	}
	if string(payload) != "NAK\n" {
		t.Fatalf("first frame = %q, want NAK\\n", payload)
	}

	var packData bytes.Buffer
	frames := 0
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read sideband: %v", err)
		}
		if isFlush {
			break
		}
		if len(payload) < 1 {
			t.Fatal("empty sideband frame")
		}
		if payload[0] == SidebandPack {
			packData.Write(payload[1:])
			frames++
		}
	}
	if frames == 0 {
		t.Fatal("no sideband pack frames received")
	}

	dec := NewPackDecoder(bytes.NewReader(packData.Bytes()))
	objs, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode pack: %v", err)
	}

	store := &LooseStore{Root: filepath.Join(dir, "objects")}
	foundCommit := false
	for _, o := range objs {
		if !store.Exists(o.Oid()) {
			t.Errorf("decoded object %s not in store", o.Oid())
		}
		if o.Oid() == commitOid {
			foundCommit = true
		}
	}
	if !foundCommit {
		t.Errorf("commit %s not in pack", commitOid)
	}
	if len(objs) != 3 {
		t.Errorf("object count = %d, want 3 (commit+tree+blob)", len(objs))
	}
}

// TestServeUploadPackNoSideband: 客户端 caps 不含 side-band-64k → NAK pkt-line + pack 直接写 out
func TestServeUploadPackNoSideband(t *testing.T) {
	dir, commitOid := makeRepoWithCommit(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s\n", commitOid))
	inw.WriteFlush()
	inw.WritePktString("done\n")

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	// 非 sideband：NAK pkt-line + pack + flush
	out := outBuf.Bytes()
	if len(out) < 8 || string(out[:8]) != "0008NAK\n" {
		end := 8
		if len(out) < 8 {
			end = len(out)
		}
		t.Fatalf("missing NAK frame: first bytes = % x", out[:end])
	}
	out = out[8:]
	if len(out) < 4 || string(out[:4]) != "PACK" {
		end := 8
		if len(out) < 8 {
			end = len(out)
		}
		t.Fatalf("output not raw pack (no sideband): first bytes = % x", out[:end])
	}
	trimmed := trimTrailingFlush(out)
	dec := NewPackDecoder(bytes.NewReader(trimmed))
	objs, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	if len(objs) != 3 {
		t.Errorf("object count = %d, want 3", len(objs))
	}
}

// trimTrailingFlush 去掉字节切片末尾的 "0000" flush 标记（若存在）。
func trimTrailingFlush(b []byte) []byte {
	if len(b) >= 4 && string(b[len(b)-4:]) == PktFlush {
		return b[:len(b)-4]
	}
	return b
}

// TestServeReceivePackRoundTrip: ref updates + packfile → ServeReceivePack → 对象写入 + ref 更新 + report-status
func TestServeReceivePackRoundTrip(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-recv-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// 构造要 push 的对象
	blob := makeBlob("pushed content\n")
	tree := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "file.txt", Oid: blob.Oid()},
	})
	commit := makeCommit(tree.Oid(), nil, "pushed commit\n")

	// 编码 packfile
	var packBuf bytes.Buffer
	enc := NewPackEncoder(&packBuf)
	if err := enc.WriteHeader(3); err != nil {
		t.Fatal(err)
	}
	enc.WriteObject(blob)
	enc.WriteObject(tree)
	enc.WriteObject(commit)
	enc.WriteTrailer()

	// 构造输入流：ref updates（首行带 caps）+ flush + packfile
	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	if err := inw.WritePktString(fmt.Sprintf("%s %s refs/heads/new\x00side-band-64k\n", ZeroOid, commit.Oid())); err != nil {
		t.Fatal(err)
	}
	if err := inw.WriteFlush(); err != nil {
		t.Fatal(err)
	}
	inBuf.Write(packBuf.Bytes())

	// ServeReceivePack
	var outBuf bytes.Buffer
	if err := ServeReceivePack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeReceivePack: %v", err)
	}

	// 验证对象写入 loose
	store := &LooseStore{Root: filepath.Join(dir, "objects")}
	for _, oid := range []Oid{blob.Oid(), tree.Oid(), commit.Oid()} {
		if !store.Exists(oid) {
			t.Errorf("object %s not written to loose store", oid)
		}
	}

	// 验证 ref 更新
	rs := NewRefStore(dir)
	got, err := rs.Get("refs/heads/new")
	if err != nil {
		t.Fatalf("Get ref: %v", err)
	}
	if got != commit.Oid() {
		t.Errorf("ref refs/heads/new = %s, want %s", got, commit.Oid())
	}

	// 验证 report-status（走 sideband ch1）
	pr := NewPktReader(&outBuf)
	var statusData bytes.Buffer
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read status sideband: %v", err)
		}
		if isFlush {
			break
		}
		if len(payload) >= 1 && payload[0] == SidebandPack {
			statusData.Write(payload[1:])
		}
	}
	statusStr := statusData.String()
	if !strings.Contains(statusStr, "unpack ok") {
		t.Errorf("report-status missing 'unpack ok': %q", statusStr)
	}
	if !strings.Contains(statusStr, "ok refs/heads/new") {
		t.Errorf("report-status missing 'ok refs/heads/new': %q", statusStr)
	}
}

// TestServeReceivePackNoSideband: 客户端 caps 不含 side-band-64k → report-status 直接 pkt-line
func TestServeReceivePackNoSideband(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-recv-nosb-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	blob := makeBlob("nosb content\n")
	tree := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "f.txt", Oid: blob.Oid()},
	})
	commit := makeCommit(tree.Oid(), nil, "nosb commit\n")

	var packBuf bytes.Buffer
	enc := NewPackEncoder(&packBuf)
	enc.WriteHeader(3)
	enc.WriteObject(blob)
	enc.WriteObject(tree)
	enc.WriteObject(commit)
	enc.WriteTrailer()

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	// 无 side-band-64k
	inw.WritePktString(fmt.Sprintf("%s %s refs/heads/master\n", ZeroOid, commit.Oid()))
	inw.WriteFlush()
	inBuf.Write(packBuf.Bytes())

	var outBuf bytes.Buffer
	if err := ServeReceivePack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeReceivePack: %v", err)
	}

	// 非 sideband：outBuf 直接是 report-status pkt-line + flush
	pr := NewPktReader(&outBuf)
	var statusData bytes.Buffer
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read status: %v", err)
		}
		if isFlush {
			break
		}
		statusData.Write(payload)
	}
	statusStr := statusData.String()
	if !strings.Contains(statusStr, "unpack ok") {
		t.Errorf("report-status missing 'unpack ok': %q", statusStr)
	}
	if !strings.Contains(statusStr, "ok refs/heads/master") {
		t.Errorf("report-status missing 'ok refs/heads/master': %q", statusStr)
	}
}

// TestServeReceivePackDeleteRef: 纯删除（无 packfile）→ report-status
func TestServeReceivePackDeleteRef(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-recv-del-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// 先创建一个 ref
	rs := NewRefStore(dir)
	rs.Update([]RefUpdate{{Name: "refs/heads/tmp", OldOid: ZeroOid, NewOid: oidA}})

	// 构造删除请求：old=oidA new=ZeroOid + flush（无 packfile）
	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("%s %s refs/heads/tmp\x00side-band-64k\n", oidA, ZeroOid))
	inw.WriteFlush()

	var outBuf bytes.Buffer
	if err := ServeReceivePack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeReceivePack: %v", err)
	}

	// ref 已删除
	if _, err := rs.Get("refs/heads/tmp"); err == nil {
		t.Errorf("ref refs/heads/tmp should be deleted")
	}

	// report-status 含 "ok refs/heads/tmp"
	pr := NewPktReader(&outBuf)
	var statusData bytes.Buffer
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read status: %v", err)
		}
		if isFlush {
			break
		}
		if len(payload) >= 1 && payload[0] == SidebandPack {
			statusData.Write(payload[1:])
		}
	}
	statusStr := statusData.String()
	if !strings.Contains(statusStr, "unpack ok") {
		t.Errorf("missing 'unpack ok': %q", statusStr)
	}
	if !strings.Contains(statusStr, "ok refs/heads/tmp") {
		t.Errorf("missing 'ok refs/heads/tmp': %q", statusStr)
	}
}

// TestHandleSSHSessionUploadPack: SSH upload-pack 单连接回环（advertise + serve）
func TestHandleSSHSessionUnsupportedArchive(t *testing.T) {
	dir, _ := makeRepoWithCommit(t)

	// git-upload-archive 本版不支持，应返回错误
	err := HandleSSHSession("git-upload-archive", dir, nil)
	if err == nil {
		t.Fatal("git-upload-archive should return error")
	}
}

func TestHandleSSHSessionUnknownService(t *testing.T) {
	dir, _ := makeRepoWithCommit(t)

	err := HandleSSHSession("git-unknown-pack", dir, nil)
	if err == nil {
		t.Fatal("unknown service should return error")
	}
}

// makeEmptyRepoWithHead 构造 InitBare 风格的空仓库：含 HEAD(symref→refs/heads/master)
// + refs/heads/ 目录但无任何 loose ref。这是 pgit InitBare 创建出的真实空仓库形态。
func makeEmptyRepoWithHead(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pgit-empty-head-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	// HEAD symref 指向尚不存在的 master
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/master\n")
	// 创建 refs/heads 目录（InitBare 会创建）
	for _, sub := range []string{"refs/heads", "refs/tags", "objects"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestAdvertiseRefsEmptyRepoWithHead: 真实 InitBare 空仓库（有 HEAD symref 无分支）
// 不应误发 "<ZeroOid> HEAD"；应发标准 "<ZeroOid> capabilities^{}\x00<caps>"。
// 覆盖 upload-pack 与 receive-pack 两种 service。
func TestAdvertiseRefsEmptyRepoWithHead(t *testing.T) {
	dir := makeEmptyRepoWithHead(t)

	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		t.Run(svc, func(t *testing.T) {
			adv, err := AdvertiseRefs(dir, svc)
			if err != nil {
				t.Fatalf("AdvertiseRefs: %v", err)
			}
			pr := NewPktReader(bytes.NewReader(adv))
			payload, isFlush, err := pr.ReadPkt()
			if err != nil {
				t.Fatalf("read first: %v", err)
			}
			if isFlush {
				t.Fatal("expected data frame, got flush")
			}
			line := string(payload)
			// 不应含 HEAD（capabilities^{} 行除外）
			if strings.Contains(line, "HEAD") {
				t.Errorf("empty adv should not contain HEAD: %q", line)
			}
			// 必须含 ZeroOid + capabilities^{}
			if !strings.HasPrefix(line, string(ZeroOid)+" capabilities^{}") {
				t.Errorf("empty adv first line = %q, want %s capabilities^{}",
					line, ZeroOid)
			}
			// 必须含 NUL + caps（之前 bug 漏了）
			if !strings.Contains(line, "\x00") {
				t.Errorf("empty adv missing NUL+caps separator: %q", line)
			}
			// caps 应含 service 对应能力
			caps := line[strings.IndexByte(line, 0)+1:]
			switch svc {
			case "git-upload-pack":
				if !strings.Contains(caps, "side-band-64k") {
					t.Errorf("upload-pack caps missing side-band-64k: %q", caps)
				}
			case "git-receive-pack":
				if !strings.Contains(caps, "report-status") {
					t.Errorf("receive-pack caps missing report-status: %q", caps)
				}
			}
			// 接下来应是 flush（仅一行 capability advertisement）
			_, isFlush, err = pr.ReadPkt()
			if err != nil || !isFlush {
				t.Fatalf("expected flush after empty adv: err=%v isFlush=%v", err, isFlush)
			}
		})
	}
}

// TestAdvertiseRefsReceivePackExcludesHead: 非空仓库 receive-pack advertisement
// 不应含 HEAD（与 cgit 一致），仅含 refs/heads/* 分支。
func TestAdvertiseRefsReceivePackExcludesHead(t *testing.T) {
	dir, _ := makeRepoWithCommit(t)

	adv, err := AdvertiseRefs(dir, "git-receive-pack")
	if err != nil {
		t.Fatalf("AdvertiseRefs: %v", err)
	}

	pr := NewPktReader(bytes.NewReader(adv))
	var lines []string
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if isFlush {
			break
		}
		line := string(payload)
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul] // 首行去掉 caps
		}
		lines = append(lines, strings.TrimRight(line, "\n"))
	}

	foundMaster, foundHead := false, false
	for _, l := range lines {
		sp := strings.IndexByte(l, ' ')
		if sp < 0 {
			continue
		}
		name := l[sp+1:]
		if name == "HEAD" {
			foundHead = true
		}
		if name == "refs/heads/master" {
			foundMaster = true
		}
	}
	if foundHead {
		t.Errorf("receive-pack adv should NOT contain HEAD: %v", lines)
	}
	if !foundMaster {
		t.Errorf("receive-pack adv should contain refs/heads/master: %v", lines)
	}
}

// TestServeReceivePackReportStatusFlush: 验证 report-status 重组后末尾含 flush-pkt。
// 缺此 flush 客户端会报「远端意外挂断了」。覆盖 sideband 与非 sideband 两种模式。
func TestServeReceivePackReportStatusFlush(t *testing.T) {
	for _, useSB := range []bool{true, false} {
		name := "noSideband"
		if useSB {
			name = "sideband"
		}
		t.Run(name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "pgit-recv-flush-*")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)

			blob := makeBlob("flush test\n")
			tree := makeTree([]TreeEntry{
				{Mode: 0o100644, Name: "f.txt", Oid: blob.Oid()},
			})
			commit := makeCommit(tree.Oid(), nil, "flush commit\n")

			var packBuf bytes.Buffer
			enc := NewPackEncoder(&packBuf)
			enc.WriteHeader(3)
			enc.WriteObject(blob)
			enc.WriteObject(tree)
			enc.WriteObject(commit)
			enc.WriteTrailer()

			var inBuf bytes.Buffer
			inw := NewPktWriter(&inBuf)
			capsLine := ""
			if useSB {
				capsLine = "side-band-64k"
			}
			if capsLine != "" {
				inw.WritePktString(fmt.Sprintf("%s %s refs/heads/dev\x00%s\n", ZeroOid, commit.Oid(), capsLine))
			} else {
				inw.WritePktString(fmt.Sprintf("%s %s refs/heads/dev\n", ZeroOid, commit.Oid()))
			}
			inw.WriteFlush()
			inBuf.Write(packBuf.Bytes())

			var outBuf bytes.Buffer
			if err := ServeReceivePack(dir, &inBuf, &outBuf); err != nil {
				t.Fatalf("ServeReceivePack: %v", err)
			}

			// 重组 report-status 数据流
			var statusData bytes.Buffer
			pr := NewPktReader(&outBuf)
			sawTrailingFlush := false
			if useSB {
				// sideband：外层 pkt-line，payload[0]==ch1 取 payload[1:]。
				// report-status 的 flush-pkt "0000" 作为 ch1 数据被重组进 statusData。
				for {
					payload, isFlush, err := pr.ReadPkt()
					if err != nil {
						t.Fatalf("read sideband: %v", err)
					}
					if isFlush {
						break // sideband 流结束
					}
					if len(payload) >= 1 && payload[0] == SidebandPack {
						statusData.Write(payload[1:])
					}
				}
				// 重组数据末尾必须含 report-status flush-pkt "0000"
				statusBytes := statusData.Bytes()
				if len(statusBytes) < 4 {
					t.Fatalf("sideband report-status too short: %d bytes", len(statusBytes))
				}
				tail := string(statusBytes[len(statusBytes)-4:])
				if tail != PktFlush {
					t.Errorf("sideband report-status missing trailing flush: last 4 = %q (want %q)\nfull: %q",
						tail, PktFlush, string(statusBytes))
				}
			} else {
				// 非 sideband：外层 pkt-line 即 report-status。
				// flush-pkt 作为独立帧出现，PktReader 读到 isFlush=true 标志结束。
				for {
					payload, isFlush, err := pr.ReadPkt()
					if err != nil {
						t.Fatalf("read status: %v", err)
					}
					if isFlush {
						sawTrailingFlush = true
						break
					}
					statusData.Write(payload)
				}
				if !sawTrailingFlush {
					t.Errorf("non-sideband report-status missing trailing flush-pkt")
				}
			}

			// 验证内容含 unpack ok + ok ref
			statusStr := statusData.String()
			if !strings.Contains(statusStr, "unpack ok") {
				t.Errorf("report-status missing 'unpack ok': %q", statusStr)
			}
			if !strings.Contains(statusStr, "ok refs/heads/dev") {
				t.Errorf("report-status missing 'ok refs/heads/dev': %q", statusStr)
			}
		})
	}
}

// TestServeReceivePackEmptyCommandList: body 仅含 flush-pkt（空命令列表，无 ref 更新、无 packfile）。
// 服务端应返回空 report-status（unpack ok + flush-pkt）而非错误，与 cgit 一致。
func TestServeReceivePackEmptyCommandList(t *testing.T) {
	dir := makeEmptyRepoWithHead(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	if err := inw.WriteFlush(); err != nil {
		t.Fatal(err)
	}

	var outBuf bytes.Buffer
	if err := ServeReceivePack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeReceivePack empty command list: %v", err)
	}

	pr := NewPktReader(&outBuf)
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read unpack status: %v", err)
	}
	if isFlush {
		t.Fatal("expected unpack status frame, got flush")
	}
	if string(payload) != "unpack ok\n" {
		t.Errorf("unpack status = %q, want %q", payload, "unpack ok\n")
	}
	_, isFlush, err = pr.ReadPkt()
	if err != nil || !isFlush {
		t.Fatalf("expected trailing flush: err=%v isFlush=%v", err, isFlush)
	}
}

// makeRepoWithTwoCommits 构造含两个 commit 的仓库：
// commit1（initial，tree1+blob1）→ commit2（second，tree2+blob1+blob2，parent=commit1）
// master 指向 commit2。返回仓库根目录、commit1 oid、commit2 oid。
func makeRepoWithTwoCommits(t *testing.T) (string, Oid, Oid) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pgit-fetch-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	store := &LooseStore{Root: filepath.Join(dir, "objects")}
	blob1 := makeBlob("hello pgit\n")
	blob2 := makeBlob("new file\n")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	tree2 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
		{Mode: 0o100644, Name: "b.txt", Oid: blob2.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial commit\n")
	commit2 := makeCommit(tree2.Oid(), []Oid{commit1.Oid()}, "second commit\n")
	writeAll(t, store, blob1, blob2, tree1, tree2, commit1, commit2)

	rs := NewRefStore(dir)
	if _, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: ZeroOid, NewOid: commit2.Oid()},
	}); err != nil {
		t.Fatalf("update refs: %v", err)
	}
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/master\n")
	return dir, commit1.Oid(), commit2.Oid()
}

// decodeSidebandPack 从 ServeUploadPack 的 sideband 输出中提取并解码 pack 对象。
// 返回解码后的对象列表。首帧必须为 NAK。
func decodeSidebandPack(t *testing.T, outBuf *bytes.Buffer) []*RawObject {
	t.Helper()
	pr := NewPktReader(outBuf)
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read NAK: %v", err)
	}
	if isFlush {
		t.Fatal("expected NAK frame, got flush")
	}
	if string(payload) != "NAK\n" {
		t.Fatalf("first frame = %q, want NAK\\n", payload)
	}

	var packData bytes.Buffer
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read sideband: %v", err)
		}
		if isFlush {
			break
		}
		if len(payload) >= 1 && payload[0] == SidebandPack {
			packData.Write(payload[1:])
		}
	}

	if packData.Len() == 0 {
		return nil
	}
	dec := NewPackDecoder(bytes.NewReader(packData.Bytes()))
	objs, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	return objs
}

// TestServeUploadPackIncrementalFetch: have 旧 commit + want 新 commit → pack 仅含增量对象
func TestServeUploadPackIncrementalFetch(t *testing.T) {
	dir, commit1Oid, commit2Oid := makeRepoWithTwoCommits(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commit2Oid))
	inw.WriteFlush()
	inw.WritePktString(fmt.Sprintf("have %s\n", commit1Oid))
	inw.WritePktString("done\n")

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	objs := decodeSidebandPack(t, &outBuf)

	// commit1 可达：commit1+tree1+blob1（have 排除）
	// commit2 可达：commit2+tree2+blob1+blob2+commit1+tree1（want 全量）
	// 差量 = commit2+tree2+blob2（commit1+tree1+blob1 被 have 排除）
	oidSet := make(map[Oid]bool)
	for _, o := range objs {
		oidSet[o.Oid()] = true
	}
	if len(objs) != 3 {
		t.Errorf("object count = %d, want 3 (commit2+tree2+blob2); got oids: %v", len(objs), oidSet)
	}

	store := &LooseStore{Root: filepath.Join(dir, "objects")}
	blob2 := makeBlob("new file\n")
	tree2 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: makeBlob("hello pgit\n").Oid()},
		{Mode: 0o100644, Name: "b.txt", Oid: blob2.Oid()},
	})
	commit2 := makeCommit(tree2.Oid(), []Oid{commit1Oid}, "second commit\n")

	for _, expected := range []Oid{commit2.Oid(), tree2.Oid(), blob2.Oid()} {
		if !oidSet[expected] {
			t.Errorf("missing expected oid %s in pack", expected)
		}
		if !store.Exists(expected) {
			t.Errorf("expected oid %s not in store", expected)
		}
	}
}

// TestServeUploadPackHaveEqualsWant: have 与 want 相同 → 仅 NAK + flush，无 PACK
func TestServeUploadPackHaveEqualsWant(t *testing.T) {
	dir, commitOid := makeRepoWithCommit(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commitOid))
	inw.WriteFlush()
	inw.WritePktString(fmt.Sprintf("have %s\n", commitOid))
	inw.WritePktString("done\n")

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	pr := NewPktReader(&outBuf)
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read NAK: %v", err)
	}
	if string(payload) != "NAK\n" {
		t.Fatalf("first frame = %q, want NAK\\n", payload)
	}
	_, isFlush, err = pr.ReadPkt()
	if err != nil {
		t.Fatalf("read flush: %v", err)
	}
	if !isFlush {
		t.Fatal("expected flush after NAK (no pack for have==want)")
	}
}

// TestServeUploadPackMultipleHaves: 多个 have 行 → 所有 have 可达对象被排除
func TestServeUploadPackMultipleHaves(t *testing.T) {
	dir, commit1Oid, commit2Oid := makeRepoWithTwoCommits(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commit2Oid))
	inw.WriteFlush()
	inw.WritePktString(fmt.Sprintf("have %s\n", commit1Oid))
	inw.WritePktString(fmt.Sprintf("have %s\n", commit2Oid))
	inw.WritePktString("done\n")

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	pr := NewPktReader(&outBuf)
	payload, _, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read NAK: %v", err)
	}
	if string(payload) != "NAK\n" {
		t.Fatalf("first frame = %q, want NAK\\n", payload)
	}
	_, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read flush: %v", err)
	}
	if !isFlush {
		t.Fatal("expected flush (have covers all wants)")
	}
}

// TestServeUploadPackHaveWithFlush: have 行之间有 flush 分隔 → flush 被跳过，have 正确收集
func TestServeUploadPackHaveWithFlush(t *testing.T) {
	dir, commit1Oid, commit2Oid := makeRepoWithTwoCommits(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commit2Oid))
	inw.WriteFlush()
	inw.WritePktString(fmt.Sprintf("have %s\n", commit1Oid))
	inw.WriteFlush()
	inw.WritePktString("done\n")

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	objs := decodeSidebandPack(t, &outBuf)
	if len(objs) != 3 {
		t.Errorf("object count = %d, want 3 (commit2+tree2+blob2)", len(objs))
	}
}

// TestServeUploadPackHaveUnrelatedBranch: have 指向无关分支 → pack 包含 want 全部可达对象
func TestServeUploadPackHaveUnrelatedBranch(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-fetch-unrel-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	store := &LooseStore{Root: filepath.Join(dir, "objects")}

	blobA := makeBlob("branch A\n")
	treeA := makeTree([]TreeEntry{{Mode: 0o100644, Name: "a.txt", Oid: blobA.Oid()}})
	commitA := makeCommit(treeA.Oid(), nil, "branch A commit\n")

	blobB := makeBlob("branch B\n")
	treeB := makeTree([]TreeEntry{{Mode: 0o100644, Name: "b.txt", Oid: blobB.Oid()}})
	commitB := makeCommit(treeB.Oid(), nil, "branch B commit\n")

	writeAll(t, store, blobA, treeA, commitA, blobB, treeB, commitB)

	rs := NewRefStore(dir)
	rs.Update([]RefUpdate{
		{Name: "refs/heads/branchA", OldOid: ZeroOid, NewOid: commitA.Oid()},
		{Name: "refs/heads/branchB", OldOid: ZeroOid, NewOid: commitB.Oid()},
	})
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/branchB\n")

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commitB.Oid()))
	inw.WriteFlush()
	inw.WritePktString(fmt.Sprintf("have %s\n", commitA.Oid()))
	inw.WritePktString("done\n")

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	objs := decodeSidebandPack(t, &outBuf)
	oidSet := make(map[Oid]bool)
	for _, o := range objs {
		oidSet[o.Oid()] = true
	}
	if !oidSet[commitB.Oid()] || !oidSet[treeB.Oid()] || !oidSet[blobB.Oid()] {
		t.Errorf("pack missing branchB objects; got oids: %v", oidSet)
	}
	if oidSet[commitA.Oid()] || oidSet[treeA.Oid()] || oidSet[blobA.Oid()] {
		t.Errorf("pack should not contain branchA objects (unrelated have); got oids: %v", oidSet)
	}
	if len(objs) != 3 {
		t.Errorf("object count = %d, want 3 (commitB+treeB+blobB only)", len(objs))
	}
}

// TestServeUploadPackNoSidebandIncremental: 非 sideband 模式增量 fetch
func TestServeUploadPackNoSidebandIncremental(t *testing.T) {
	dir, commit1Oid, commit2Oid := makeRepoWithTwoCommits(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s\n", commit2Oid))
	inw.WriteFlush()
	inw.WritePktString(fmt.Sprintf("have %s\n", commit1Oid))
	inw.WritePktString("done\n")

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	out := outBuf.Bytes()
	if len(out) < 8 || string(out[:8]) != "0008NAK\n" {
		t.Fatalf("missing NAK frame")
	}
	out = out[8:]
	trimmed := trimTrailingFlush(out)
	dec := NewPackDecoder(bytes.NewReader(trimmed))
	objs, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	if len(objs) != 3 {
		t.Errorf("object count = %d, want 3 (commit2+tree2+blob2)", len(objs))
	}
}

// TestServeUploadPackHaveFlushNoDone: HTTP stateless_rpc 中间请求。
// 请求体 want+flush+have+flush（无 done）→ 响应仅 NAK，无 PACK，无 flush。
// 客户端 fetch-pack 在 have flush 后 get_ack 读 NAK，响应体结束（EOF）。
// 修复前：pgit 不响应 have flush，EOF 后发 NAK+PACK+flush，客户端读到 sideband PACK 报
// "expected ACK/NAK, got '?PACK'"。
func TestServeUploadPackHaveFlushNoDone(t *testing.T) {
	dir, commit1Oid, commit2Oid := makeRepoWithTwoCommits(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commit2Oid))
	inw.WriteFlush()
	inw.WritePktString(fmt.Sprintf("have %s\n", commit1Oid))
	inw.WriteFlush()
	// 无 done：模拟 HTTP 中间请求（have flush 后请求体结束）

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	// 响应仅一个 NAK 帧，无 PACK，无 flush
	pr := NewPktReader(&outBuf)
	payload, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read NAK: %v", err)
	}
	if isFlush {
		t.Fatal("expected NAK frame, got flush")
	}
	if string(payload) != "NAK\n" {
		t.Fatalf("frame = %q, want NAK\\n", payload)
	}
	// 之后应是 EOF（无 PACK，无 flush）
	if _, _, err := pr.ReadPkt(); err != io.EOF {
		t.Errorf("expected EOF after NAK (no pack, no flush), got err=%v", err)
	}
}

// TestServeUploadPackHTTPMultiRequestIncremental: 模拟 HTTP stateless_rpc 多 POST 增量 fetch。
// POST1: want+flush+have+flush（无 done）→ 仅 NAK。
// POST2: want+flush+have+done → NAK+PACK+flush。
// 验证 POST2 的 pack 含增量对象（have 排除 commit1 可达对象，仅 commit2+tree2+blob2）。
func TestServeUploadPackHTTPMultiRequestIncremental(t *testing.T) {
	dir, commit1Oid, commit2Oid := makeRepoWithTwoCommits(t)

	// POST 1: have flush 中间请求
	var in1 bytes.Buffer
	inw1 := NewPktWriter(&in1)
	inw1.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commit2Oid))
	inw1.WriteFlush()
	inw1.WritePktString(fmt.Sprintf("have %s\n", commit1Oid))
	inw1.WriteFlush()

	var out1 bytes.Buffer
	if err := ServeUploadPack(dir, &in1, &out1); err != nil {
		t.Fatalf("POST1 ServeUploadPack: %v", err)
	}
	pr1 := NewPktReader(&out1)
	payload, isFlush, err := pr1.ReadPkt()
	if err != nil || isFlush || string(payload) != "NAK\n" {
		t.Fatalf("POST1 response = %q (isFlush=%v, err=%v), want NAK\\n", payload, isFlush, err)
	}
	if _, _, err := pr1.ReadPkt(); err != io.EOF {
		t.Errorf("POST1 expected EOF after NAK, got err=%v", err)
	}

	// POST 2: done 最终请求
	var in2 bytes.Buffer
	inw2 := NewPktWriter(&in2)
	inw2.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commit2Oid))
	inw2.WriteFlush()
	inw2.WritePktString(fmt.Sprintf("have %s\n", commit1Oid))
	inw2.WritePktString("done\n")

	var out2 bytes.Buffer
	if err := ServeUploadPack(dir, &in2, &out2); err != nil {
		t.Fatalf("POST2 ServeUploadPack: %v", err)
	}

	objs := decodeSidebandPack(t, &out2)
	oidSet := make(map[Oid]bool)
	for _, o := range objs {
		oidSet[o.Oid()] = true
	}
	if len(objs) != 3 {
		t.Errorf("POST2 object count = %d, want 3 (commit2+tree2+blob2); oids: %v", len(objs), oidSet)
	}
	// commit1 可达对象不应出现（被 have 排除）
	if oidSet[commit1Oid] {
		t.Errorf("POST2 pack should not contain have-reachable commit1 %s", commit1Oid)
	}
}

// TestServeUploadPackHaveFlushThenDone: 单请求 have flush + done（SSH 流式或单 POST 场景）。
// want+flush+have+flush+done → NAK（have flush 响应）+ NAK（done 响应）+ sideband PACK + flush。
// 验证 pack 含增量对象，且 NAK 数量为 2（对齐 fetch-pack get_ack 次数）。
func TestServeUploadPackHaveFlushThenDone(t *testing.T) {
	dir, commit1Oid, commit2Oid := makeRepoWithTwoCommits(t)

	var inBuf bytes.Buffer
	inw := NewPktWriter(&inBuf)
	inw.WritePktString(fmt.Sprintf("want %s side-band-64k\n", commit2Oid))
	inw.WriteFlush()
	inw.WritePktString(fmt.Sprintf("have %s\n", commit1Oid))
	inw.WriteFlush()
	inw.WritePktString("done\n")

	var outBuf bytes.Buffer
	if err := ServeUploadPack(dir, &inBuf, &outBuf); err != nil {
		t.Fatalf("ServeUploadPack: %v", err)
	}

	// 首帧 NAK（have flush 响应）
	pr := NewPktReader(&outBuf)
	payload, isFlush, err := pr.ReadPkt()
	if err != nil || isFlush || string(payload) != "NAK\n" {
		t.Fatalf("first NAK (have flush) = %q (isFlush=%v, err=%v)", payload, isFlush, err)
	}

	// 剩余流：第二个 NAK（done 响应）作为 decodeSidebandPack 的首帧，之后 sideband PACK
	objs := decodeSidebandPack(t, &outBuf)
	if len(objs) != 3 {
		t.Errorf("object count = %d, want 3 (commit2+tree2+blob2)", len(objs))
	}
	oidSet := make(map[Oid]bool)
	for _, o := range objs {
		oidSet[o.Oid()] = true
	}
	if oidSet[commit1Oid] {
		t.Errorf("pack should not contain have-reachable commit1 %s", commit1Oid)
	}
}
