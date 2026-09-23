package git

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 构造 n 个互不相同的 blob 的合法 pack。
func buildPack(t *testing.T, n int, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := NewPackEncoder(&buf)
	if err := enc.WriteHeader(n); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		content := append([]byte(fmt.Sprintf("obj-%04d-", i)), bytes.Repeat([]byte{byte('a' + i%26)}, size)...)
		if err := enc.WriteObject(NewRawObject(ObjBlob, content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.WriteTrailer(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// DecodeTo 必须逐个对象写盘且不持有全部内容：
// 用内存 store 计数，并确认解码器不保留对象副本。
func TestPackDecoderDecodeToStreamsObjects(t *testing.T) {
	const n, size = 64, 64 << 10 // 64 × 64KiB = 4MiB
	pack := buildPack(t, n, size)

	store := newMemStore()
	dec := NewPackDecoder(bytes.NewReader(pack))
	written, err := dec.DecodeTo(store)
	if err != nil {
		t.Fatalf("DecodeTo: %v", err)
	}
	if written != n {
		t.Fatalf("written = %d, want %d", written, n)
	}
	if len(store.objs) != n {
		t.Fatalf("store objects = %d, want %d", len(store.objs), n)
	}
	// 流式模式不收集对象
	if dec.collected != nil || len(dec.order) != 0 {
		t.Fatalf("DecodeTo must not retain objects (collected=%d order=%d)", len(dec.collected), len(dec.order))
	}
}

// DecodeTo 与 Decode 对同一 pack 结果一致（内容与 oid）。
func TestPackDecoderStreamMatchesCollect(t *testing.T) {
	pack := buildPack(t, 16, 1024)

	collected, err := NewPackDecoder(bytes.NewReader(pack)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	if _, err := NewPackDecoder(bytes.NewReader(pack)).DecodeTo(store); err != nil {
		t.Fatal(err)
	}
	if len(collected) != len(store.objs) {
		t.Fatalf("count mismatch: %d vs %d", len(collected), len(store.objs))
	}
	for _, o := range collected {
		got, ok := store.objs[o.Oid()]
		if !ok || !bytes.Equal(got.Content, o.Content) {
			t.Fatalf("object %s mismatch between Decode and DecodeTo", o.Oid())
		}
	}
}

// 流式解码的常驻堆应远小于「全量收集」模式（证明对象不再随 pack 大小驻留）。
// 度量方式：抓取 f() 返回瞬间（GC 之前）的 HeapAlloc 峰值增量。
func TestPackDecoderStreamMemoryBounded(t *testing.T) {
	if raceEnabled {
		t.Skip("内存度量在 -race 下受影子内存干扰，仅在普通模式下断言")
	}
	const n, size = 32, 256 << 10 // 32 × 256KiB = 8MiB 解压后内容
	pack := buildPack(t, n, size)

	peak := func(f func()) uint64 {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		var after runtime.MemStats
		runtime.ReadMemStats(&after) // 不 GC：取返回瞬间的驻留量
		if after.HeapAlloc < before.HeapAlloc {
			return 0
		}
		return after.HeapAlloc - before.HeapAlloc
	}

	var collected []*RawObject
	collectPeak := peak(func() {
		objs, err := NewPackDecoder(bytes.NewReader(pack)).Decode()
		if err != nil {
			t.Fatal(err)
		}
		collected = objs // 保持可达，防止被优化/回收
	})
	streamPeak := peak(func() {
		if _, err := NewPackDecoder(bytes.NewReader(pack)).DecodeTo(discardStore{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("peak heap: collect=%d bytes, stream=%d bytes (uncompressed=%d, objs=%d)",
		collectPeak, streamPeak, n*size, len(collected))

	if collectPeak < uint64(n*size)/2 {
		t.Fatalf("collect mode peak unexpectedly small (%d), premise invalid", collectPeak)
	}
	if streamPeak > collectPeak/2 {
		t.Fatalf("streaming decode retains too much: stream=%d collect=%d", streamPeak, collectPeak)
	}
}

// oidOfFirstBlob 从 pack 中解出第一个 blob 的 oid（构造 CAS 用 ref 更新行）。
func oidOfFirstBlob(pack []byte) Oid {
	objs, err := NewPackDecoder(bytes.NewReader(pack)).Decode()
	if err != nil || len(objs) == 0 {
		return ZeroOid
	}
	return objs[0].Oid()
}

// buildPackIncompressible 构造含伪随机内容的 pack（zlib 压不动，确保体积可控）。
func buildPackIncompressible(t *testing.T, n int, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := NewPackEncoder(&buf)
	if err := enc.WriteHeader(n); err != nil {
		t.Fatal(err)
	}
	rnd := uint32(12345)
	for i := 0; i < n; i++ {
		content := make([]byte, size)
		for j := range content {
			rnd = rnd*1664525 + 1013904223
			content[j] = byte(rnd >> 24)
		}
		if err := enc.WriteObject(NewRawObject(ObjBlob, content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.WriteTrailer(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// discardStore 写入即丢弃（对象内容不驻留）。
type discardStore struct{}

func (discardStore) Read(oid Oid) (*RawObject, error) {
	return nil, fmt.Errorf("discard store has no objects")
}
func (discardStore) Exists(oid Oid) bool               { return false }
func (discardStore) Write(obj *RawObject) (Oid, error) { return obj.Oid(), nil }

// 上限：SetMaxReceivePackBytes 生效于 receive-pack —— pack 超限时不得更新 ref，
// 且必须回 report-status（unpack error）而不是让客户端挂死。
func TestMaxReceivePackBytesLimit(t *testing.T) {
	const old = defaultMaxReceivePackBytes
	defer SetMaxReceivePackBytes(old)

	SetMaxReceivePackBytes(1024) // 1KiB
	pack := buildPackIncompressible(t, 4, 4096)

	req := &bytes.Buffer{}
	pwReq := NewPktWriter(req)
	if err := pwReq.WritePktString(fmt.Sprintf("%s %s refs/heads/master\x00report-status side-band-64k\n", ZeroOid, oidOfFirstBlob(pack))); err != nil {
		t.Fatal(err)
	}
	if err := pwReq.WriteFlush(); err != nil {
		t.Fatal(err)
	}
	req.Write(pack)

	dir := t.TempDir()
	if err := InitBareRepoForTest(dir); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	if err := ServeReceivePack(dir, bytes.NewReader(req.Bytes()), out); err != nil {
		t.Fatalf("oversized pack should be rejected via report-status, got error: %v", err)
	}
	// 输出应包含 unpack error 与 ng
	text := out.String()
	if !strings.Contains(text, "unpack error") {
		t.Fatalf("expected unpack error in report-status, got %q", text)
	}
	if !strings.Contains(text, "ng refs/heads/master") {
		t.Fatalf("expected ng for ref, got %q", text)
	}
	// ref 不得被创建
	rs := NewRefStore(dir)
	if oid, err := rs.Get("refs/heads/master"); err == nil && !oid.IsZero() {
		t.Fatalf("ref should not be updated, got %s", oid)
	}
}

// InitBareRepoForTest 在 dir 下建立最小裸仓库结构（HEAD/refs/objects）。
func InitBareRepoForTest(dir string) error {
	for _, sub := range []string{"objects/info", "objects/pack", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o777); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/master\n"), 0o644); err != nil {
		return err
	}
	return nil
}
