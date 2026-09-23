package git

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"fmt"
	"testing"
)

// memStore 是 ObjectStore 的内存实现，用于验证协议层与浏览 API
// 是否已与「松散对象文件存储」解耦（不依赖任何磁盘路径）。
type memStore struct {
	objs map[Oid]*RawObject
}

func newMemStore() *memStore {
	return &memStore{objs: map[Oid]*RawObject{}}
}

func (s *memStore) Read(oid Oid) (*RawObject, error) {
	o, ok := s.objs[oid]
	if !ok {
		return nil, fmt.Errorf("memstore: object %s not found", oid)
	}
	return o, nil
}

func (s *memStore) Exists(oid Oid) bool {
	_, ok := s.objs[oid]
	return ok
}

func (s *memStore) Write(obj *RawObject) (Oid, error) {
	oid := obj.Oid()
	s.objs[oid] = obj
	return oid, nil
}

var _ ObjectStore = (*memStore)(nil)

// 浏览 API（TreeAt/BlobAt/CommitLog）与可达性遍历（CollectReachable）
// 必须能跑在任意 ObjectStore 实现上。
func TestObjectStoreAbstraction(t *testing.T) {
	store := newMemStore()

	blob := makeBlob("hello\n")
	nested := makeBlob("nested\n")
	subTree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "b.txt", Oid: nested.Oid()}})
	rootTree := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob.Oid()},
		{Mode: 0o040000, Name: "docs", Oid: subTree.Oid()},
	})
	commit := makeCommit(rootTree.Oid(), nil, "init\n")

	for _, o := range []*RawObject{blob, nested, subTree, rootTree, commit} {
		if _, err := store.Write(o); err != nil {
			t.Fatalf("write %s: %v", o.Oid(), err)
		}
	}

	entries, err := TreeAt(store, rootTree.Oid(), "docs")
	if err != nil {
		t.Fatalf("TreeAt: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "b.txt" {
		t.Fatalf("TreeAt entries = %+v", entries)
	}

	b, err := BlobAt(store, rootTree.Oid(), "docs/b.txt")
	if err != nil {
		t.Fatalf("BlobAt: %v", err)
	}
	if string(b.Content) != "nested\n" {
		t.Fatalf("BlobAt content = %q", b.Content)
	}

	log, err := CommitLog(store, commit.Oid(), 10)
	if err != nil {
		t.Fatalf("CommitLog: %v", err)
	}
	if len(log) != 1 || log[0].Subject != "init" {
		t.Fatalf("CommitLog = %+v", log)
	}

	objs, err := CollectReachable(store, []Oid{commit.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	if len(objs) != 5 {
		t.Fatalf("CollectReachable objects = %d, want 5", len(objs))
	}

	// have 过滤同样只依赖接口
	incremental, err := CollectReachable(store, []Oid{commit.Oid()}, rootTree.Oid())
	if err != nil {
		t.Fatalf("CollectReachable(have): %v", err)
	}
	if len(incremental) != 1 || incremental[0].Oid() != commit.Oid() {
		t.Fatalf("incremental objects = %d (want just the commit)", len(incremental))
	}
}

// PackDecoder 的 REF_DELTA 回查同样只要求 ObjectStore（可用内存实现充当 base 来源）。
func TestPackDecoderWithObjectStore(t *testing.T) {
	base := makeBlob("base content for delta\n")
	store := newMemStore()
	if _, err := store.Write(base); err != nil {
		t.Fatal(err)
	}

	target := makeBlob("base content for delta, extended\n")
	delta, err := EncodeDelta(base.Content, target.Content)
	if err != nil {
		t.Fatalf("EncodeDelta: %v", err)
	}
	pack := buildRefDeltaPack(t, base.Oid(), delta)

	dec := NewPackDecoder(bytes.NewReader(pack), store)
	objs, err := dec.Decode()
	if err != nil {
		t.Fatalf("Decode with ObjectStore base: %v", err)
	}
	if len(objs) != 1 || string(objs[0].Content) != string(target.Content) {
		t.Fatalf("decoded object mismatch: %+v", objs)
	}
}

// buildRefDeltaPack 手工构造含单个 REF_DELTA 的 pack（base 不在 pack 内）。
func buildRefDeltaPack(t *testing.T, baseOid Oid, delta []byte) []byte {
	t.Helper()
	var body []byte
	body = append(body, "PACK"...)
	body = append(body, 0, 0, 0, 2) // version 2
	body = append(body, 0, 0, 0, 1) // 1 object

	// type=REF_DELTA(7) + size=len(delta)，变长编码
	size := uint64(len(delta))
	b := byte((packObjRefDelta << 4) | (byte(size) & 0x0f))
	size >>= 4
	for size > 0 {
		b |= 0x80
		body = append(body, b)
		b = byte(size & 0x7f)
		size >>= 7
	}
	body = append(body, b)

	body = append(body, oidToBytes(baseOid)...)

	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(delta); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	body = append(body, zbuf.Bytes()...)

	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}
