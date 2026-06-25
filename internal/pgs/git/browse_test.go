package git

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// setupBrowseRepo 构造一个含子目录与 annotated tag 的测试仓库目录。
// 结构：master → commit1 → tree1{file.txt(blob1), dir/{nested.txt(blob2)}}；tag v1.0 → commit1。
func setupBrowseRepo(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "pgit-browse-*")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"objects", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	store := &LooseStore{Root: filepath.Join(root, "objects")}

	blob1 := makeBlob("hello\n")
	blob2 := makeBlob("nested\n")
	tree2 := makeTree([]TreeEntry{{Mode: 0o100644, Name: "nested.txt", Oid: blob2.Oid()}})
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "file.txt", Oid: blob1.Oid()},
		{Mode: treeMode, Name: "dir", Oid: tree2.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial commit\n")
	tag1 := makeTag(commit1.Oid(), ObjCommit, "v1.0")
	writeAll(t, store, blob1, blob2, tree2, tree1, commit1, tag1)

	mustWrite := func(p, c string) {
		if err := os.WriteFile(filepath.Join(root, p), []byte(c), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("HEAD", "ref: refs/heads/master\n")
	mustWrite("refs/heads/master", string(commit1.Oid())+"\n")
	mustWrite("refs/tags/v1.0", string(tag1.Oid())+"\n")
	return root
}

func TestResolveTreeIsh(t *testing.T) {
	root := setupBrowseRepo(t)
	defer os.RemoveAll(root)
	store := &LooseStore{Root: filepath.Join(root, "objects")}

	// 由 commit1 算期望 tree oid
	cobj, err := store.Read(readRef(t, root, "refs/heads/master"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCommit(cobj.Content)
	if err != nil {
		t.Fatal(err)
	}
	wantTree := c.Tree

	// ref 名（short）
	commitOid, treeOid, err := ResolveTreeIsh(root, "master")
	if err != nil {
		t.Fatalf("ResolveTreeIsh(master): %v", err)
	}
	if treeOid != wantTree {
		t.Errorf("master treeOid = %s, want %s", treeOid, wantTree)
	}
	if commitOid == "" {
		t.Errorf("master commitOid should be non-empty")
	}

	// HEAD
	_, treeOid2, err := ResolveTreeIsh(root, "HEAD")
	if err != nil {
		t.Fatalf("ResolveTreeIsh(HEAD): %v", err)
	}
	if treeOid2 != wantTree {
		t.Errorf("HEAD treeOid = %s, want %s", treeOid2, wantTree)
	}

	// 完整 commit oid
	_, treeOid3, err := ResolveTreeIsh(root, string(commitOid))
	if err != nil {
		t.Fatalf("ResolveTreeIsh(commitOid): %v", err)
	}
	if treeOid3 != wantTree {
		t.Errorf("commitOid treeOid = %s, want %s", treeOid3, wantTree)
	}

	// 直接 tree oid → commitOid 为空
	co, to, err := ResolveTreeIsh(root, string(wantTree))
	if err != nil {
		t.Fatalf("ResolveTreeIsh(treeOid): %v", err)
	}
	if to != wantTree {
		t.Errorf("treeOid direct = %s, want %s", to, wantTree)
	}
	if co != "" {
		t.Errorf("direct tree should have empty commitOid, got %s", co)
	}

	// tag → 解引用到 commit 的 tree
	_, treeOid4, err := ResolveTreeIsh(root, "v1.0")
	if err != nil {
		t.Fatalf("ResolveTreeIsh(v1.0): %v", err)
	}
	if treeOid4 != wantTree {
		t.Errorf("v1.0 treeOid = %s, want %s", treeOid4, wantTree)
	}

	// 不存在的 ref
	if _, _, err := ResolveTreeIsh(root, "nope"); err == nil {
		t.Fatal("expected error for unknown ref")
	}
}

func TestTreeAtBlobAt(t *testing.T) {
	root := setupBrowseRepo(t)
	defer os.RemoveAll(root)
	store := &LooseStore{Root: filepath.Join(root, "objects")}
	_, treeOid, err := ResolveTreeIsh(root, "master")
	if err != nil {
		t.Fatal(err)
	}

	// 顶层 tree：file.txt + dir
	entries, err := TreeAt(store, treeOid, "")
	if err != nil {
		t.Fatalf("TreeAt(root): %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("top entries = %d, want 2", len(entries))
	}
	names := []string{entries[0].Name, entries[1].Name}
	sort.Strings(names)
	if names[0] != "dir" || names[1] != "file.txt" {
		t.Fatalf("top names = %v", names)
	}

	// 子目录 dir → nested.txt
	sub, err := TreeAt(store, treeOid, "dir")
	if err != nil {
		t.Fatalf("TreeAt(dir): %v", err)
	}
	if len(sub) != 1 || sub[0].Name != "nested.txt" {
		t.Fatalf("dir entries = %v", sub)
	}

	// blob 顶层
	b, err := BlobAt(store, treeOid, "file.txt")
	if err != nil {
		t.Fatalf("BlobAt(file.txt): %v", err)
	}
	if string(b.Content) != "hello\n" {
		t.Errorf("file.txt content = %q", b.Content)
	}

	// blob 嵌套
	b2, err := BlobAt(store, treeOid, "dir/nested.txt")
	if err != nil {
		t.Fatalf("BlobAt(dir/nested.txt): %v", err)
	}
	if string(b2.Content) != "nested\n" {
		t.Errorf("nested.txt content = %q", b2.Content)
	}

	// 不存在路径
	if _, err := BlobAt(store, treeOid, "nope.txt"); err == nil {
		t.Fatal("expected error for missing blob")
	}
	if _, err := TreeAt(store, treeOid, "nope"); err == nil {
		t.Fatal("expected error for missing tree path")
	}

	// 空路径 BlobAt 报错
	if _, err := BlobAt(store, treeOid, ""); err == nil {
		t.Fatal("expected error for empty blob path")
	}
}

func TestForEachRefs(t *testing.T) {
	root := setupBrowseRepo(t)
	defer os.RemoveAll(root)

	refs, err := ForEachRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	// master + v1.0
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2: %+v", len(refs), refs)
	}
	byName := map[string]RefInfo{}
	for _, r := range refs {
		byName[r.Name] = r
	}
	m, ok := byName["master"]
	if !ok {
		t.Fatalf("master not in refs: %+v", byName)
	}
	if m.Type != "commit" {
		t.Errorf("master type = %q, want commit", m.Type)
	}
	if m.Author != "Test" || m.Email != "test@pgit.dev" {
		t.Errorf("master author = %q %q", m.Author, m.Email)
	}
	if m.Timestamp != 1700000000 {
		t.Errorf("master ts = %d, want 1700000000", m.Timestamp)
	}
	if m.Subject != "initial commit" {
		t.Errorf("master subject = %q", m.Subject)
	}
	if m.FullName != "refs/heads/master" {
		t.Errorf("master fullname = %q", m.FullName)
	}

	v, ok := byName["v1.0"]
	if !ok {
		t.Fatalf("v1.0 not in refs: %+v", byName)
	}
	if v.Type != "tag" {
		t.Errorf("v1.0 type = %q, want tag", v.Type)
	}
	if v.Subject != "tag message" {
		t.Errorf("v1.0 subject = %q", v.Subject)
	}
	if v.FullName != "refs/tags/v1.0" {
		t.Errorf("v1.0 fullname = %q", v.FullName)
	}
}

// readRef 读 loose ref 文件内容（trim）。
func readRef(t *testing.T, root, name string) Oid {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return Oid(s)
}
