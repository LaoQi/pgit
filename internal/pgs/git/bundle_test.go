package git

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bundleTestRepo 构造含 2 个提交 + 1 个 tag + HEAD=master 的仓库。
func bundleTestRepo(t *testing.T) (string, []Oid) {
	t.Helper()
	dir := t.TempDir()
	store := &LooseStore{Root: filepath.Join(dir, "objects")}
	blob := makeBlob("hello bundle\n")
	tree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "a.txt", Oid: blob.Oid()}})
	c1 := makeCommit(tree.Oid(), nil, "one\n")
	c2 := makeCommit(tree.Oid(), []Oid{c1.Oid()}, "two\n")
	writeAll(t, store, blob, tree, c1, c2)
	rs := NewRefStore(dir)
	if _, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: ZeroOid, NewOid: c2.Oid()},
		{Name: "refs/tags/v1", OldOid: ZeroOid, NewOid: c1.Oid()},
	}); err != nil {
		t.Fatalf("update refs: %v", err)
	}
	if err := rs.SetHead("refs/heads/master"); err != nil {
		t.Fatalf("set head: %v", err)
	}
	return dir, []Oid{c1.Oid(), c2.Oid()}
}

func TestBundleHeaderAndRoundTrip(t *testing.T) {
	src, commits := bundleTestRepo(t)

	var buf bytes.Buffer
	info, err := WriteBundle(&buf, src)
	if err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if info.Objects != 4 { // 1 blob + 1 tree + 2 commit
		t.Errorf("Objects = %d, want 4", info.Objects)
	}
	if info.Head != "refs/heads/master" {
		t.Errorf("Head = %q, want refs/heads/master", info.Head)
	}
	if len(info.Refs) != 2 {
		t.Fatalf("Refs = %d, want 2", len(info.Refs))
	}
	if buf.Len() != int(info.Bytes) {
		t.Errorf("Bytes = %d, want %d", info.Bytes, buf.Len())
	}
	if !strings.HasPrefix(buf.String(), bundleMagic+"\n") {
		t.Errorf("bundle magic missing: %q", buf.String()[:32])
	}

	hdr, _, err := ReadBundle(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}
	if hdr.Version != "v2" {
		t.Errorf("Version = %q, want v2", hdr.Version)
	}
	if hdr.HeadOid != commits[1] {
		t.Errorf("HeadOid = %s, want %s", hdr.HeadOid, commits[1])
	}
	if len(hdr.Refs) != 2 {
		t.Fatalf("header refs = %d, want 2", len(hdr.Refs))
	}

	// 恢复到一个全新仓库
	bundlePath := filepath.Join(t.TempDir(), "snap.bundle")
	if err := os.WriteFile(bundlePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	res, err := RestoreBundle(bundlePath, dst, info.Head)
	if err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	if res.Objects != 4 {
		t.Errorf("restored objects = %d, want 4", res.Objects)
	}
	if res.Head != "refs/heads/master" {
		t.Errorf("restored head = %q", res.Head)
	}
	assertRefEquals(t, dst, "refs/heads/master", commits[1])
	assertRefEquals(t, dst, "refs/tags/v1", commits[0])
	rs := NewRefStore(dst)
	if head, err := rs.Head(); err != nil || head != "refs/heads/master" {
		t.Errorf("restored HEAD = %q err=%v", head, err)
	}
	// 解包后的对象能被浏览/遍历（WalkReachable 读得到内容）
	store := NewObjectStore(dst)
	metas, err := WalkReachable(store, []Oid{commits[1]}, nil, nil)
	if err != nil || len(metas) != 4 {
		t.Errorf("walk restored = %d err=%v, want 4", len(metas), err)
	}
}

func TestRestoreBundleOverwritesExistingRef(t *testing.T) {
	src, commits := bundleTestRepo(t)
	var buf bytes.Buffer
	if _, err := WriteBundle(&buf, src); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(t.TempDir(), "snap.bundle")
	if err := os.WriteFile(bundlePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// 目标仓库把 master 指向 tag 的提交（模拟"被重写后的新历史"）
	dst, _ := bundleTestRepo(t)
	rs := NewRefStore(dst)
	if _, err := rs.Update([]RefUpdate{{Name: "refs/heads/master", OldOid: commits[1], NewOid: commits[0]}}); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	res, err := RestoreBundle(bundlePath, dst, "refs/heads/master")
	if err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	if len(res.Refs) != 2 {
		t.Errorf("refs restored = %v, want 2", res.Refs)
	}
	assertRefEquals(t, dst, "refs/heads/master", commits[1])
}

func TestRestoreBundleDetectsCorruption(t *testing.T) {
	src, _ := bundleTestRepo(t)
	var buf bytes.Buffer
	if _, err := WriteBundle(&buf, src); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	// 截断 pack 尾部（trailer SHA1 缺失/内容不完整）
	broken := data[:len(data)-10]
	bundlePath := filepath.Join(t.TempDir(), "broken.bundle")
	if err := os.WriteFile(bundlePath, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBundle(bundlePath, t.TempDir(), ""); err == nil {
		t.Fatal("expected error for truncated bundle")
	}

	// 翻转 pack 中一个字节（对象内容损坏 → 逐对象 SHA1 校验失败）
	mut := append([]byte(nil), data...)
	mut[len(mut)-25] ^= 0xff
	badPath := filepath.Join(t.TempDir(), "bad.bundle")
	if err := os.WriteFile(badPath, mut, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBundle(badPath, t.TempDir(), ""); err == nil {
		t.Fatal("expected error for corrupted bundle")
	}
}

func TestReadBundleRejectsUnsafeHeader(t *testing.T) {
	oid := strings.Repeat("a", 40)
	cases := []string{
		"not a bundle\n",
		bundleMagic + "\n" + oid + " ../../etc/passwd\n\n",
		bundleMagic + "\n" + oid + " /abs/path\n\n",
		bundleMagic + "\n",
		bundleMagic + "\n-" + oid + " refs/heads/master\n\n",
	}
	for i, c := range cases {
		if _, _, err := ReadBundle(strings.NewReader(c)); err == nil {
			t.Errorf("case %d: expected error for %q", i, c)
		}
	}
}

func TestWriteBundleEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/master\n")
	var buf bytes.Buffer
	if _, err := WriteBundle(&buf, dir); err == nil {
		t.Fatal("expected error for repo without refs")
	}
}
