package git

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- pkt-line 读写 ---

func TestPktLineRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPktWriter(&buf)
	frames := [][]byte{
		[]byte("hello pgit\n"),
		[]byte("second frame"),
		[]byte("a"),
		bytes.Repeat([]byte("x"), 1000),
	}
	for i, f := range frames {
		if err := pw.WritePkt(f); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := pw.WriteFlush(); err != nil {
		t.Fatal(err)
	}

	pr := NewPktReader(&buf)
	for i, want := range frames {
		got, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if isFlush {
			t.Fatalf("frame %d: unexpected flush", i)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d: got %q want %q", i, got, want)
		}
	}
	_, isFlush, err := pr.ReadPkt()
	if err != nil {
		t.Fatalf("read flush: %v", err)
	}
	if !isFlush {
		t.Fatal("expected flush, got data")
	}
}

func TestPktLineSpecialMarkers(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPktWriter(&buf)
	pw.WritePktString("a")
	pw.WriteFlush()
	buf.WriteString(PktDelim)
	buf.WriteString(PktResponseEnd)
	pw.WritePktString("b")

	pr := NewPktReader(&buf)
	// "a"
	g, fl, err := pr.ReadPkt()
	if err != nil || fl || string(g) != "a" {
		t.Fatalf("a: g=%q fl=%v err=%v", g, fl, err)
	}
	// flush
	_, fl, err = pr.ReadPkt()
	if err != nil || !fl {
		t.Fatalf("flush: fl=%v err=%v", fl, err)
	}
	// delim → isFlush=true
	_, fl, err = pr.ReadPkt()
	if err != nil || !fl {
		t.Fatalf("delim: fl=%v err=%v", fl, err)
	}
	// response-end → isFlush=true
	_, fl, err = pr.ReadPkt()
	if err != nil || !fl {
		t.Fatalf("resp-end: fl=%v err=%v", fl, err)
	}
	// "b"
	g, fl, err = pr.ReadPkt()
	if err != nil || fl || string(g) != "b" {
		t.Fatalf("b: g=%q fl=%v err=%v", g, fl, err)
	}
}

func TestPktLineTooLong(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPktWriter(&buf)
	if err := pw.WritePkt(bytes.Repeat([]byte("a"), maxPktPayload+1)); err == nil {
		t.Fatal("expected error for too-long payload")
	}
}

// --- sideband 分帧重组 ---

func TestSidebandReassembly(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPktWriter(&buf)
	sw := NewSidebandWriter(pw, SidebandPack)
	data := bytes.Repeat([]byte("pgit"), 20000) // 80000 bytes > SidebandMaxPayload
	if _, err := sw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := pw.WriteFlush(); err != nil {
		t.Fatal(err)
	}

	pr := NewPktReader(&buf)
	var reassembled []byte
	frames := 0
	for {
		payload, isFlush, err := pr.ReadPkt()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if isFlush {
			break
		}
		if len(payload) < 1 {
			t.Fatal("empty sideband frame")
		}
		if payload[0] != SidebandPack {
			t.Fatalf("expected pack channel, got %d", payload[0])
		}
		reassembled = append(reassembled, payload[1:]...)
		frames++
	}
	if frames < 2 {
		t.Fatalf("expected multiple frames, got %d", frames)
	}
	if !bytes.Equal(reassembled, data) {
		t.Fatalf("reassembled mismatch: got %d bytes want %d", len(reassembled), len(data))
	}
}

// --- delta 应用 ---

func TestApplyDelta(t *testing.T) {
	base := []byte("hello world, this is the base content for delta")
	// target: world -> WORLD
	target := []byte("hello WORLD, this is the base content for delta")
	// delta: copy base[0:6] "hello ", insert "WORLD", copy base[11:] ", this..."
	pre := 6
	mid := []byte("WORLD")
	sufStart := 11
	sufLen := len(base) - sufStart

	var d []byte
	d = append(d, encodeVarintLE(uint64(len(base)))...)
	d = append(d, encodeVarintLE(uint64(len(target)))...)
	// copy pre: op 0x91(offset b0 + size b0), offset=0, size=pre
	d = append(d, 0x91, 0x00, byte(pre))
	// insert mid
	d = append(d, byte(len(mid)))
	d = append(d, mid...)
	// copy suffix: op 0x91, offset=sufStart, size=sufLen
	d = append(d, 0x91, byte(sufStart), byte(sufLen))

	got, err := ApplyDelta(base, d)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("delta result mismatch:\n got %q\nwant %q", got, target)
	}
}

func TestApplyDeltaSrcSizeMismatch(t *testing.T) {
	base := []byte("abc")
	// srcSize varint(999)=[0xe7,0x07], tgtSize=3, insert 'x'
	d := []byte{0xe7, 0x07, 0x03, 0x01, 'x'}
	if _, err := ApplyDelta(base, d); err == nil {
		t.Fatal("expected src size mismatch error")
	}
}

func TestApplyDeltaIllegalZero(t *testing.T) {
	base := []byte("abc")
	// srcSize=3, tgtSize=3, then 0x00 (illegal)
	d := []byte{0x03, 0x03, 0x00}
	if _, err := ApplyDelta(base, d); err == nil {
		t.Fatal("expected illegal 0x00 error")
	}
}

// --- pack encode/decode 回环（无 delta）---

func TestPackEncodeDecodeRoundTrip(t *testing.T) {
	objs := []*RawObject{
		NewRawObject(ObjBlob, []byte("hello pgit\n")),
		NewRawObject(ObjBlob, []byte("")),
		NewRawObject(ObjBlob, bytes.Repeat([]byte("z"), 500)),
		NewRawObject(ObjTree, mustHex(t, rootTreeHex)),
		NewRawObject(ObjCommit, mustHex(t, commitHex)),
	}
	var buf bytes.Buffer
	enc := NewPackEncoder(&buf)
	if err := enc.WriteHeader(len(objs)); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if err := enc.WriteObject(o); err != nil {
			t.Fatalf("write obj %s: %v", o.Oid(), err)
		}
	}
	if err := enc.WriteTrailer(); err != nil {
		t.Fatal(err)
	}

	dec := NewPackDecoder(bytes.NewReader(buf.Bytes()))
	got, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(objs) {
		t.Fatalf("count %d want %d", len(got), len(objs))
	}
	for i := range objs {
		if got[i].Type != objs[i].Type {
			t.Errorf("obj %d type %s want %s", i, got[i].Type, objs[i].Type)
		}
		if got[i].Size != objs[i].Size {
			t.Errorf("obj %d size %d want %d", i, got[i].Size, objs[i].Size)
		}
		if !bytes.Equal(got[i].Content, objs[i].Content) {
			t.Errorf("obj %d content mismatch", i)
		}
		if got[i].Oid() != objs[i].Oid() {
			t.Errorf("obj %d oid %s want %s", i, got[i].Oid(), objs[i].Oid())
		}
	}
}

func TestPackDecodeTrailerMismatch(t *testing.T) {
	obj := NewRawObject(ObjBlob, []byte("hello"))
	var buf bytes.Buffer
	enc := NewPackEncoder(&buf)
	enc.WriteHeader(1)
	enc.WriteObject(obj)
	enc.WriteTrailer()
	// 破坏 trailer
	b := buf.Bytes()
	b[len(b)-1] ^= 0xff
	if _, err := NewPackDecoder(bytes.NewReader(b)).Decode(); err == nil {
		t.Fatal("expected trailer mismatch error")
	}
}

// --- pack 解码（手工构造 OFS_DELTA，验证 byte-offset 解析）---

func TestPackDecodeOfsDelta(t *testing.T) {
	base := []byte("base content for ofs delta test, long enough")
	extra := []byte(" EXTRA TAIL DATA")
	target := append(append([]byte{}, base...), extra...)

	// delta: copy whole base, then insert extra
	var delta []byte
	delta = append(delta, encodeVarintLE(uint64(len(base)))...)
	delta = append(delta, encodeVarintLE(uint64(len(target)))...)
	delta = append(delta, 0x91, 0x00, byte(len(base))) // copy base[0:len]
	delta = append(delta, byte(len(extra)))
	delta = append(delta, extra...)

	var pack bytes.Buffer
	pack.WriteString(packMagic)
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], packVersion)
	pack.Write(tmp[:])
	binary.BigEndian.PutUint32(tmp[:], 2) // 2 objects
	pack.Write(tmp[:])

	// obj0: blob base
	writePackObjHeader(&pack, packObjBlob, uint64(len(base)))
	pack.Write(zlibBytes(base))

	// obj1: ofs-delta
	obj1Start := pack.Len()
	ofs := obj1Start - 12 // base type 字节在偏移 12
	writePackObjHeader(&pack, packObjOfsDelta, uint64(len(delta)))
	pack.Write(encodeOfsDelta(uint64(ofs)))
	pack.Write(zlibBytes(delta))

	// trailer
	h := sha1.Sum(pack.Bytes())
	pack.Write(h[:])

	dec := NewPackDecoder(bytes.NewReader(pack.Bytes()))
	got, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("count %d want 2", len(got))
	}
	if got[0].Type != ObjBlob || string(got[0].Content) != string(base) {
		t.Fatalf("obj0: type=%s content=%q", got[0].Type, got[0].Content)
	}
	if got[1].Type != ObjBlob {
		t.Fatalf("obj1: type=%s want blob", got[1].Type)
	}
	if !bytes.Equal(got[1].Content, target) {
		t.Fatalf("obj1 content:\n got %q\nwant %q", got[1].Content, target)
	}
	wantOid := NewRawObject(ObjBlob, target).Oid()
	if got[1].Oid() != wantOid {
		t.Fatalf("obj1 oid %s want %s", got[1].Oid(), wantOid)
	}
}

// --- 真实 git pack 解码 ---

func TestPackDecodeRealGitPack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found, skipping real pack test")
	}
	dir, err := os.MkdirTemp("", "pgit-realpack-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	runGit := func(args ...string) ([]byte, error) {
		return exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	}
	if _, err := runGit("init", "-q"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := runGit("config", "user.name", "Test"); err != nil {
		t.Fatalf("config name: %v", err)
	}
	if _, err := runGit("config", "user.email", "test@pgit.dev"); err != nil {
		t.Fatalf("config email: %v", err)
	}

	// 两版大文件差异小，迫使 git 产生 delta
	big := strings.Repeat("line of text content here\n", 60)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(big+"version one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("add", "a.txt"); err != nil {
		t.Fatalf("add1: %v", err)
	}
	if _, err := runGit("commit", "-q", "-m", "one"); err != nil {
		t.Fatalf("commit1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(big+"version two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("add", "a.txt"); err != nil {
		t.Fatalf("add2: %v", err)
	}
	if _, err := runGit("commit", "-q", "-m", "two"); err != nil {
		t.Fatalf("commit2: %v", err)
	}

	// 期望对象 oid 集合（rev-list --objects --all）
	revOut, err := runGit("rev-list", "--objects", "--all")
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	want := map[Oid]bool{}
	for _, line := range strings.Split(string(revOut), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		want[Oid(strings.Fields(line)[0])] = true
	}
	if len(want) == 0 {
		t.Fatal("no objects from rev-list")
	}

	// 生成 pack：rev-list 输出喂给 pack-objects --stdout
	poCmd := exec.Command("git", "-C", dir, "pack-objects", "--stdout")
	poCmd.Stdin = bytes.NewReader(revOut)
	var poOut, poErr bytes.Buffer
	poCmd.Stdout = &poOut
	poCmd.Stderr = &poErr
	if err := poCmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v\nstderr: %s", err, poErr.String())
	}
	if poOut.Len() == 0 {
		t.Fatal("empty pack output")
	}

	dec := NewPackDecoder(bytes.NewReader(poOut.Bytes()))
	got, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	gotSet := map[Oid]bool{}
	for _, o := range got {
		gotSet[o.Oid()] = true
	}
	for oid := range want {
		if !gotSet[oid] {
			t.Errorf("decoded pack missing oid %s", oid)
		}
	}
	for oid := range gotSet {
		if !want[oid] {
			t.Errorf("decoded pack has extra oid %s", oid)
		}
	}
}

// --- helpers ---

func encodeVarintLE(v uint64) []byte {
	var out []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		out = append(out, c)
		if v == 0 {
			break
		}
	}
	return out
}

// encodeOfsDelta 编码 git ofs-delta 偏移（与 readOfsDelta 互逆，与 git get_delta_base 一致）
func encodeOfsDelta(off uint64) []byte {
	var buf [16]byte
	pos := len(buf) - 1
	buf[pos] = byte(off) & 0x7f
	tmp := off >> 7
	for tmp != 0 {
		pos--
		tmp--
		buf[pos] = 0x80 | byte(tmp&0x7f)
		tmp >>= 7
	}
	return append([]byte(nil), buf[pos:]...)
}

func zlibBytes(data []byte) []byte {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	zw.Write(data)
	zw.Close()
	return b.Bytes()
}

func writePackObjHeader(w *bytes.Buffer, pt byte, size uint64) {
	b := byte((pt << 4) | (byte(size) & 0x0f))
	size >>= 4
	for size > 0 {
		b |= 0x80
		w.WriteByte(b)
		b = byte(size & 0x7f)
		size >>= 7
	}
	w.WriteByte(b)
}
