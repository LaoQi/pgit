package pgs

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgit/internal/pgs/git"
)

// buildBrowseRepo 用 InitBare 建裸仓库，再写入 blob/tree/commit 与 master ref。
// 结构同 browse_test：master → commit → tree{file.txt, dir/{nested.txt}}。
func buildBrowseRepo(t *testing.T, name string) *Repository {
	t.Helper()
	dir, err := os.MkdirTemp("", "pgit-repobrowse-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	GitRoot = dir

	repo, err := InitBare(name, "browse test")
	if err != nil {
		t.Fatal(err)
	}
	store := &git.LooseStore{Root: filepath.Join(repo.Path(), "objects")}

	blob1 := git.NewRawObject(git.ObjBlob, []byte("hello\n"))
	blob2 := git.NewRawObject(git.ObjBlob, []byte("nested\n"))
	tree2 := git.NewRawObject(git.ObjTree, treeBody([]git.TreeEntry{
		{Mode: 0o100644, Name: "nested.txt", Oid: blob2.Oid()},
	}))
	tree1 := git.NewRawObject(git.ObjTree, treeBody([]git.TreeEntry{
		{Mode: 0o100644, Name: "file.txt", Oid: blob1.Oid()},
		{Mode: 0o040000, Name: "dir", Oid: tree2.Oid()},
	}))
	commitBody := fmt.Sprintf("tree %s\nauthor Test <test@pgit.dev> 1700000000 +0800\ncommitter Test <test@pgit.dev> 1700000000 +0800\n\ninitial\n",
		tree1.Oid())
	commit := git.NewRawObject(git.ObjCommit, []byte(commitBody))
	for _, o := range []*git.RawObject{blob1, blob2, tree2, tree1, commit} {
		if _, err := store.Write(o); err != nil {
			t.Fatalf("write %s: %v", o.Oid(), err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo.Path(), "refs/heads/master"),
		[]byte(string(commit.Oid())+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	return repo
}

// treeBody 编码 tree entry 列表为 git tree 对象内容。
func treeBody(entries []git.TreeEntry) []byte {
	var buf []byte
	for _, e := range entries {
		buf = append(buf, []byte(fmt.Sprintf("%o %s", e.Mode, e.Name))...)
		buf = append(buf, 0)
		b, _ := hex.DecodeString(string(e.Oid))
		buf = append(buf, b...)
	}
	return buf
}

func TestRepoTree(t *testing.T) {
	repo := buildBrowseRepo(t, "browsetree")

	nodes, err := repo.Tree("master", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("top nodes = %d, want 2", len(nodes))
	}
	// 找 dir 与 file.txt
	var dirNode, fileNode *TreeNode
	for i := range nodes {
		switch nodes[i].Name {
		case "dir":
			dirNode = &nodes[i]
		case "file.txt":
			fileNode = &nodes[i]
		}
	}
	if dirNode == nil || dirNode.Type != "tree" {
		t.Errorf("dir node missing or wrong type: %+v", dirNode)
	}
	if fileNode == nil || fileNode.Type != "blob" {
		t.Errorf("file.txt node missing or wrong type: %+v", fileNode)
	}

	// 子目录
	sub, err := repo.Tree("master", "dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 || sub[0].Name != "nested.txt" || sub[0].Type != "blob" {
		t.Errorf("dir entries = %+v", sub)
	}

	// 不存在的 ref
	if _, err := repo.Tree("nope", ""); err == nil {
		t.Fatal("expected error for unknown ref")
	}
}

func TestRepoBlob(t *testing.T) {
	repo := buildBrowseRepo(t, "browseblob")

	rc, err := repo.Blob("master", "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "hello\n" {
		t.Errorf("file.txt = %q", data)
	}

	// 嵌套
	rc2, err := repo.Blob("master", "dir/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc2.Close()
	data2, _ := io.ReadAll(rc2)
	if string(data2) != "nested\n" {
		t.Errorf("nested.txt = %q", data2)
	}

	if _, err := repo.Blob("master", "missing.txt"); err == nil {
		t.Fatal("expected error for missing blob")
	}
}

func TestRepoArchive(t *testing.T) {
	repo := buildBrowseRepo(t, "browsearchive")

	rc, err := repo.Archive("master")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip open: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	want := []string{
		"browsearchive/file.txt",
		"browsearchive/dir/nested.txt",
	}
	for _, w := range want {
		if !names[w] {
			t.Errorf("zip missing %q; have %v", w, names)
		}
	}
	// 确认无目录条目（仅文件）
	for n := range names {
		if strings.HasSuffix(n, "/") {
			t.Errorf("unexpected dir entry %q", n)
		}
	}
}

func TestRepoForEachRef(t *testing.T) {
	repo := buildBrowseRepo(t, "browsefor")

	refs, err := repo.ForEachRef()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(refs))
	}
	r := refs[0]
	if r.Name != "master" {
		t.Errorf("ref name = %q, want master", r.Name)
	}
	if r.Type != "commit" {
		t.Errorf("ref type = %q, want commit", r.Type)
	}
	if r.Author != "Test" || r.Email != "test@pgit.dev" {
		t.Errorf("author = %q %q", r.Author, r.Email)
	}
	if r.Timestamp != 1700000000 {
		t.Errorf("ts = %d", r.Timestamp)
	}
	if r.Subject != "initial" {
		t.Errorf("subject = %q", r.Subject)
	}
}
