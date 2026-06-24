package git

import (
	"encoding/hex"
	"os"
	"testing"
)

// 测试数据由真实 git 生成（git 2.54），本测试本身不依赖 git 二进制。
//
//   empty blob          oid e69de29bb2d1d6434b8b29ae775ad8c2e48c5391
//   "hello pgit\n" blob oid a5ccb972673562ef5bad1a6cced799f9d71a796b
//
//   root tree           oid 04af7178a88c09441b737eec0cf0879c8571d22b
//     含两条 entry：100644 hello.txt -> 上面的 blob
//                   40000  sub      -> 子 tree 9aa0820e953336d54faf31c96b40883012b8a27c
//
//   commit              oid 8ac2ce9315a60cf4ab8e50e7c5a3c9c1b2473653
//     tree 04af7178a88c09441b737eec0cf0879c8571d22b
//     author Alice <alice@pgit.dev> 1700000000 +0800
//     committer Bob <bob@pgit.dev> 1700000000 +0800
//
//     initial commit

// root tree 对象的原始（未压缩）内容 hex，含二进制 sha，故以 hex 硬编码
const rootTreeHex = "3130303634342068656c6c6f2e74787400a5ccb972673562ef5bad1a6cce" +
	"d799f9d71a796b343030303020737562009aa0820e953336d54faf31c96b" +
	"40883012b8a27c"

// commit 对象原始内容 hex（全 ASCII，但同样以 hex 保留以便对照 git 输出）
const commitHex = "747265652030346166373137386138386330393434316237333765656330" +
	"6366303837396338353731643232620a617574686f7220416c696365203c" +
	"616c69636540706769742e6465763e2031373030303030303030202b3038" +
	"30300a636f6d6d697474657220426f62203c626f6240706769742e646576" +
	"3e2031373030303030303030202b303830300a0a696e697469616c20636f" +
	"6d6d69740a"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}
	return b
}

func TestLooseStoreWriteReadBlob(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-loose-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store := &LooseStore{Root: dir}
	content := []byte("hello pgit\n")
	obj := NewRawObject(ObjBlob, content)

	oid, err := store.Write(obj)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	wantOid := Oid("a5ccb972673562ef5bad1a6cced799f9d71a796b")
	if oid != wantOid {
		t.Fatalf("oid = %s, want %s", oid, wantOid)
	}
	if !store.Exists(oid) {
		t.Fatalf("Exists(%s) = false after write", oid)
	}

	got, err := store.Read(oid)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Type != ObjBlob {
		t.Fatalf("type = %s, want blob", got.Type)
	}
	if got.Size != len(content) {
		t.Fatalf("size = %d, want %d", got.Size, len(content))
	}
	if string(got.Content) != string(content) {
		t.Fatalf("content mismatch: got %q want %q", got.Content, content)
	}
	if got.Oid() != wantOid {
		t.Fatalf("recompute oid = %s, want %s", got.Oid(), wantOid)
	}
}

func TestLooseStoreWriteIdempotent(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-loose-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store := &LooseStore{Root: dir}
	obj := NewRawObject(ObjBlob, []byte("hello pgit\n"))

	oid1, err := store.Write(obj)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	path1 := store.Path(oid1)
	info1, err := os.Stat(path1)
	if err != nil {
		t.Fatalf("stat after first write: %v", err)
	}
	size1 := info1.Size()

	// 第二次写入同一对象应幂等成功，不报错、不改动文件
	oid2, err := store.Write(obj)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if oid1 != oid2 {
		t.Fatalf("second write oid = %s, want %s", oid2, oid1)
	}
	info2, err := os.Stat(path1)
	if err != nil {
		t.Fatalf("stat after second write: %v", err)
	}
	if info2.Size() != size1 {
		t.Fatalf("file size changed on idempotent write: %d -> %d", size1, info2.Size())
	}
}

func TestParseCommit(t *testing.T) {
	content := mustHex(t, commitHex)
	c, err := ParseCommit(content)
	if err != nil {
		t.Fatalf("ParseCommit: %v", err)
	}
	if c.Tree != Oid("04af7178a88c09441b737eec0cf0879c8571d22b") {
		t.Fatalf("tree = %s, want 04af...", c.Tree)
	}
	if len(c.Parents) != 0 {
		t.Fatalf("parents = %v, want none", c.Parents)
	}
	if c.Author.Name != "Alice" {
		t.Errorf("author name = %q, want Alice", c.Author.Name)
	}
	if c.Author.Email != "alice@pgit.dev" {
		t.Errorf("author email = %q, want alice@pgit.dev", c.Author.Email)
	}
	if c.Author.Timestamp != 1700000000 {
		t.Errorf("author ts = %d, want 1700000000", c.Author.Timestamp)
	}
	if c.Author.Offset != 480 {
		t.Errorf("author offset = %d, want 480 (+0800)", c.Author.Offset)
	}
	if c.Committer.Name != "Bob" {
		t.Errorf("committer name = %q, want Bob", c.Committer.Name)
	}
	if c.Committer.Email != "bob@pgit.dev" {
		t.Errorf("committer email = %q, want bob@pgit.dev", c.Committer.Email)
	}
	if c.Committer.Offset != 480 {
		t.Errorf("committer offset = %d, want 480", c.Committer.Offset)
	}
	if c.Message != "initial commit\n" {
		t.Errorf("message = %q, want %q", c.Message, "initial commit\n")
	}
}

func TestParseTree(t *testing.T) {
	content := mustHex(t, rootTreeHex)
	tr, err := ParseTree(content)
	if err != nil {
		t.Fatalf("ParseTree: %v", err)
	}
	if len(tr.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(tr.Entries))
	}

	// entry 0: 100644 hello.txt -> blob
	e0 := tr.Entries[0]
	if e0.Mode != 0o100644 {
		t.Errorf("e0 mode = %o, want 100644", e0.Mode)
	}
	if e0.Name != "hello.txt" {
		t.Errorf("e0 name = %q, want hello.txt", e0.Name)
	}
	if e0.Oid != Oid("a5ccb972673562ef5bad1a6cced799f9d71a796b") {
		t.Errorf("e0 oid = %s, want hello blob", e0.Oid)
	}

	// entry 1: 40000 sub -> subtree
	e1 := tr.Entries[1]
	if e1.Mode != 0o40000 {
		t.Errorf("e1 mode = %o, want 40000", e1.Mode)
	}
	if e1.Name != "sub" {
		t.Errorf("e1 name = %q, want sub", e1.Name)
	}
	if e1.Oid != Oid("9aa0820e953336d54faf31c96b40883012b8a27c") {
		t.Errorf("e1 oid = %s, want subtree", e1.Oid)
	}
}

func TestComputeOid(t *testing.T) {
	cases := []struct {
		name    string
		objType string
		content []byte
		want    Oid
	}{
		{"empty blob", "blob", []byte{}, Oid("e69de29bb2d1d6434b8b29ae775ad8c2e48c5391")},
		{"hello blob", "blob", []byte("hello pgit\n"), Oid("a5ccb972673562ef5bad1a6cced799f9d71a796b")},
		{"root tree", "tree", mustHex(t, rootTreeHex), Oid("04af7178a88c09441b737eec0cf0879c8571d22b")},
		{"commit", "commit", mustHex(t, commitHex), Oid("8ac2ce9315a60cf4ab8e50e7c5a3c9c1b2473653")},
	}
	for _, c := range cases {
		got := ComputeOid(c.objType, c.content)
		if got != c.want {
			t.Errorf("ComputeOid(%s) = %s, want %s", c.name, got, c.want)
		}
		// RawObject.Oid() 应与 ComputeOid 一致
		ro := NewRawObject(ObjectType(c.objType), c.content)
		if ro.Oid() != c.want {
			t.Errorf("RawObject.Oid(%s) = %s, want %s", c.name, ro.Oid(), c.want)
		}
	}
}
