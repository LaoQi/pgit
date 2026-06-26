package git

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"testing"
)

// oidToBytes 将 40 字符 hex oid 转为 20 字节二进制（tree entry 用）。
func oidToBytes(o Oid) []byte {
	b, err := hex.DecodeString(string(o))
	if err != nil {
		panic(fmt.Sprintf("oidToBytes %s: %v", o, err))
	}
	return b
}

// makeBlob 构造 blob 对象。
func makeBlob(content string) *RawObject {
	return NewRawObject(ObjBlob, []byte(content))
}

// makeTree 构造 tree 对象：每条 entry 拼为 "<mode> <name>\0<20bin sha>"。
func makeTree(entries []TreeEntry) *RawObject {
	var buf []byte
	for _, e := range entries {
		buf = append(buf, []byte(fmt.Sprintf("%o %s", e.Mode, e.Name))...)
		buf = append(buf, 0)
		buf = append(buf, oidToBytes(e.Oid)...)
	}
	return NewRawObject(ObjTree, buf)
}

// makeCommit 构造 commit 对象。
func makeCommit(tree Oid, parents []Oid, msg string) *RawObject {
	var buf []byte
	buf = append(buf, []byte(fmt.Sprintf("tree %s\n", tree))...)
	for _, p := range parents {
		buf = append(buf, []byte(fmt.Sprintf("parent %s\n", p))...)
	}
	buf = append(buf, []byte("author Test <test@pgit.dev> 1700000000 +0800\n")...)
	buf = append(buf, []byte("committer Test <test@pgit.dev> 1700000000 +0800\n")...)
	buf = append(buf, '\n')
	buf = append(buf, []byte(msg)...)
	return NewRawObject(ObjCommit, buf)
}

// makeTag 构造 annotated tag 对象。
func makeTag(target Oid, targetType ObjectType, name string) *RawObject {
	var buf []byte
	buf = append(buf, []byte(fmt.Sprintf("object %s\n", target))...)
	buf = append(buf, []byte(fmt.Sprintf("type %s\n", targetType))...)
	buf = append(buf, []byte(fmt.Sprintf("tag %s\n", name))...)
	buf = append(buf, []byte("tagger Test <test@pgit.dev> 1700000000 +0800\n")...)
	buf = append(buf, '\n')
	buf = append(buf, []byte("tag message\n")...)
	return NewRawObject(ObjTag, buf)
}

// writeAll 将多个对象写入 store 并返回 oid→对象 映射。
func writeAll(t *testing.T, store *LooseStore, objs ...*RawObject) map[Oid]*RawObject {
	t.Helper()
	m := make(map[Oid]*RawObject)
	for _, o := range objs {
		oid, err := store.Write(o)
		if err != nil {
			t.Fatalf("write %s: %v", o.Oid(), err)
		}
		m[oid] = o
	}
	return m
}

// oidSet 将结果列表转为 oid 集合，便于断言。
func oidSet(objs []*RawObject) map[Oid]bool {
	m := make(map[Oid]bool, len(objs))
	for _, o := range objs {
		m[o.Oid()] = true
	}
	return m
}

// assertOids 检查 results 恰好包含 wants 中的所有 oid（无多无少）。
func assertOids(t *testing.T, results []*RawObject, wants ...Oid) {
	t.Helper()
	got := oidSet(results)
	if len(got) != len(wants) {
		t.Fatalf("got %d objects, want %d; got=%v want=%v", len(got), len(wants), got, wants)
	}
	for _, w := range wants {
		if !got[w] {
			t.Fatalf("missing oid %s in results; got=%v", w, got)
		}
	}
}

// TestReachFromCommit: blob1+blob2+tree1+commit1 → 从 commit1 出发应得 4 个对象
func TestReachFromCommit(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	blob2 := makeBlob("world")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
		{Mode: 0o100644, Name: "b.txt", Oid: blob2.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")

	writeAll(t, store, blob1, blob2, tree1, commit1)

	results, err := CollectReachable(store, []Oid{commit1.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, commit1.Oid(), tree1.Oid(), blob1.Oid(), blob2.Oid())
}

// TestReachFromTag: tag1 → commit1 → tree1 → blob1+blob2，应得 5 个对象
func TestReachFromTag(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	blob2 := makeBlob("world")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
		{Mode: 0o100644, Name: "b.txt", Oid: blob2.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")
	tag1 := makeTag(commit1.Oid(), ObjCommit, "v1")

	writeAll(t, store, blob1, blob2, tree1, commit1, tag1)

	results, err := CollectReachable(store, []Oid{tag1.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, tag1.Oid(), commit1.Oid(), tree1.Oid(), blob1.Oid(), blob2.Oid())
}

// TestReachCommitChain: commit2(parent=commit1) → 5 个对象
func TestReachCommitChain(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	blob2 := makeBlob("world")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
		{Mode: 0o100644, Name: "b.txt", Oid: blob2.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")
	commit2 := makeCommit(tree1.Oid(), []Oid{commit1.Oid()}, "second\n")

	writeAll(t, store, blob1, blob2, tree1, commit1, commit2)

	results, err := CollectReachable(store, []Oid{commit2.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, commit2.Oid(), commit1.Oid(), tree1.Oid(), blob1.Oid(), blob2.Oid())
}

// TestReachDedup: rootOids 含重复或交叉可达，结果不重复
func TestReachDedup(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	blob2 := makeBlob("world")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
		{Mode: 0o100644, Name: "b.txt", Oid: blob2.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")
	commit2 := makeCommit(tree1.Oid(), []Oid{commit1.Oid()}, "second\n")

	writeAll(t, store, blob1, blob2, tree1, commit1, commit2)

	// 从 commit2 + commit1 + commit2（重复）+ tree1（交叉）出发
	results, err := CollectReachable(store, []Oid{commit2.Oid(), commit1.Oid(), commit2.Oid(), tree1.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	// 应恰好 5 个：commit2, commit1, tree1, blob1, blob2（无重复）
	assertOids(t, results, commit2.Oid(), commit1.Oid(), tree1.Oid(), blob1.Oid(), blob2.Oid())
	if len(results) != 5 {
		t.Fatalf("result count = %d, want 5 (no dups)", len(results))
	}
}

// TestReachGitlinkNotEnqueued: 含 gitlink 的 tree，gitlink 指向不存在的 oid，不应报错
func TestReachGitlinkNotEnqueued(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	fakeSubmoduleOid := Oid("1234567890123456789012345678901234567890") // 不存在的 commit oid
	treeWithGitlink := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
		{Mode: 0o160000, Name: "sub", Oid: fakeSubmoduleOid}, // gitlink，不入队
	})
	commit1 := makeCommit(treeWithGitlink.Oid(), nil, "initial\n")

	writeAll(t, store, blob1, treeWithGitlink, commit1)
	// 注意：fakeSubmoduleOid 对应的对象未写入 store

	results, err := CollectReachable(store, []Oid{commit1.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable should not error on gitlink: %v", err)
	}
	// 应得 3 个：commit1, treeWithGitlink, blob1（gitlink 不入队，fakeSubmoduleOid 不读）
	assertOids(t, results, commit1.Oid(), treeWithGitlink.Oid(), blob1.Oid())
	if len(results) != 3 {
		t.Fatalf("result count = %d, want 3 (gitlink not enqueued)", len(results))
	}
}

// TestReachZeroOidSkipped: rootOids 含 ZeroOid 应跳过不报错
func TestReachZeroOidSkipped(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")

	writeAll(t, store, blob1, tree1, commit1)

	results, err := CollectReachable(store, []Oid{ZeroOid, commit1.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable should skip ZeroOid: %v", err)
	}
	assertOids(t, results, commit1.Oid(), tree1.Oid(), blob1.Oid())
}

// TestReachMissingObjectError: 指向不存在（非 ZeroOid）的 oid 应报错
func TestReachMissingObjectError(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	missing := Oid("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	_, err = CollectReachable(store, []Oid{missing})
	if err == nil {
		t.Fatalf("CollectReachable should error on missing oid")
	}
}

// TestReachRefsAlias: CollectReachableRefs 与 CollectReachable 行为一致
func TestReachRefsAlias(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")

	writeAll(t, store, blob1, tree1, commit1)

	results, err := CollectReachableRefs(store, []Oid{commit1.Oid()})
	if err != nil {
		t.Fatalf("CollectReachableRefs: %v", err)
	}
	assertOids(t, results, commit1.Oid(), tree1.Oid(), blob1.Oid())
}

// TestReachNestedTrees: 多层子目录 tree 递归可达
func TestReachNestedTrees(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("inner")
	subTree := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "inner.txt", Oid: blob1.Oid()},
	})
	rootTree := makeTree([]TreeEntry{
		{Mode: 0o040000, Name: "sub", Oid: subTree.Oid()},
	})
	commit1 := makeCommit(rootTree.Oid(), nil, "initial\n")

	writeAll(t, store, blob1, subTree, rootTree, commit1)

	results, err := CollectReachable(store, []Oid{commit1.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, commit1.Oid(), rootTree.Oid(), subTree.Oid(), blob1.Oid())
}

// TestReachTagChain: tag 指向另一个 tag（链式），用 visited 防循环
func TestReachTagChain(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")
	tagInner := makeTag(commit1.Oid(), ObjCommit, "v1")
	tagOuter := makeTag(tagInner.Oid(), ObjTag, "v1-annotated")

	writeAll(t, store, blob1, tree1, commit1, tagInner, tagOuter)

	results, err := CollectReachable(store, []Oid{tagOuter.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, tagOuter.Oid(), tagInner.Oid(), commit1.Oid(), tree1.Oid(), blob1.Oid())
}

// TestReachOrderDetermined: 结果按 BFS 访问顺序，root 先于其引用
func TestReachOrderDetermined(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")

	writeAll(t, store, blob1, tree1, commit1)

	results, err := CollectReachable(store, []Oid{commit1.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	// BFS：commit1 第一个，tree1 第二个（commit 引用），blob1 第三个（tree 引用）
	if len(results) != 3 {
		t.Fatalf("len = %d, want 3", len(results))
	}
	if results[0].Oid() != commit1.Oid() {
		t.Errorf("results[0] = %s, want commit1", results[0].Oid())
	}
	if results[1].Oid() != tree1.Oid() {
		t.Errorf("results[1] = %s, want tree1", results[1].Oid())
	}
	if results[2].Oid() != blob1.Oid() {
		t.Errorf("results[2] = %s, want blob1", results[2].Oid())
	}
}

// TestReachEmptyRoots: 空 rootOids → 空结果不报错
func TestReachEmptyRoots(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	results, err := CollectReachable(store, nil)
	if err != nil {
		t.Fatalf("CollectReachable(nil): %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("len = %d, want 0", len(results))
	}
}

// TestReachAllTypesSorted: 仅作为编译期保证：结果可排序（供调试用）
func TestReachAllTypesSorted(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")
	tag1 := makeTag(commit1.Oid(), ObjCommit, "v1")

	writeAll(t, store, blob1, tree1, commit1, tag1)

	results, err := CollectReachable(store, []Oid{tag1.Oid()})
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	// 按 oid 排序，确保结果集稳定可比较
	sorted := make([]*RawObject, len(results))
	copy(sorted, results)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Oid() < sorted[j].Oid() })
	if len(sorted) != 4 {
		t.Fatalf("len = %d, want 4 (tag+commit+tree+blob)", len(sorted))
	}
}

// TestReachWithHaveFullCoverage: have 完全覆盖 want 可达 → 空结果
func TestReachWithHaveFullCoverage(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")

	writeAll(t, store, blob1, tree1, commit1)

	results, err := CollectReachable(store, []Oid{commit1.Oid()}, commit1.Oid())
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty result (have covers all), got %d: %v", len(results), oidSet(results))
	}
}

// TestReachWithHavePartialCoverage: have 指向旧 commit，want 指向新 commit → 仅返回增量
func TestReachWithHavePartialCoverage(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	blob2 := makeBlob("world")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	tree2 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
		{Mode: 0o100644, Name: "b.txt", Oid: blob2.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")
	commit2 := makeCommit(tree2.Oid(), []Oid{commit1.Oid()}, "second\n")

	writeAll(t, store, blob1, blob2, tree1, tree2, commit1, commit2)

	results, err := CollectReachable(store, []Oid{commit2.Oid()}, commit1.Oid())
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, commit2.Oid(), tree2.Oid(), blob2.Oid())
}

// TestReachWithHaveSharedTree: commit1 和 commit2 共享 tree → have commit1 仅排除 commit1+tree+blob
func TestReachWithHaveSharedTree(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("shared")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "first\n")
	commit2 := makeCommit(tree1.Oid(), []Oid{commit1.Oid()}, "second\n")

	writeAll(t, store, blob1, tree1, commit1, commit2)

	results, err := CollectReachable(store, []Oid{commit2.Oid()}, commit1.Oid())
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, commit2.Oid())
}

// TestReachWithHaveNoCoverage: have 指向无关对象 → 结果等于全量 CollectReachable
func TestReachWithHaveNoCoverage(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blobA := makeBlob("branchA")
	treeA := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blobA.Oid()},
	})
	commitA := makeCommit(treeA.Oid(), nil, "branch A\n")

	blobB := makeBlob("branchB")
	treeB := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "b.txt", Oid: blobB.Oid()},
	})
	commitB := makeCommit(treeB.Oid(), nil, "branch B\n")

	writeAll(t, store, blobA, treeA, commitA, blobB, treeB, commitB)

	results, err := CollectReachable(store, []Oid{commitB.Oid()}, commitA.Oid())
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, commitB.Oid(), treeB.Oid(), blobB.Oid())
}

// TestReachWithHaveLinearChain: 线性 3 commit 链，have=commit1, want=commit3 → 仅增量
func TestReachWithHaveLinearChain(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("v1")
	blob2 := makeBlob("v2")
	blob3 := makeBlob("v3")
	tree1 := makeTree([]TreeEntry{{Mode: 0o100644, Name: "f.txt", Oid: blob1.Oid()}})
	tree2 := makeTree([]TreeEntry{{Mode: 0o100644, Name: "f.txt", Oid: blob2.Oid()}})
	tree3 := makeTree([]TreeEntry{{Mode: 0o100644, Name: "f.txt", Oid: blob3.Oid()}})
	commit1 := makeCommit(tree1.Oid(), nil, "v1\n")
	commit2 := makeCommit(tree2.Oid(), []Oid{commit1.Oid()}, "v2\n")
	commit3 := makeCommit(tree3.Oid(), []Oid{commit2.Oid()}, "v3\n")

	writeAll(t, store, blob1, blob2, blob3, tree1, tree2, tree3, commit1, commit2, commit3)

	results, err := CollectReachable(store, []Oid{commit3.Oid()}, commit1.Oid())
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, commit3.Oid(), commit2.Oid(), tree3.Oid(), tree2.Oid(), blob3.Oid(), blob2.Oid())
}

// TestReachWithHaveMissingOid: have 指向不存在的 oid → 忽略，等价于无 have
func TestReachWithHaveMissingOid(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")

	writeAll(t, store, blob1, tree1, commit1)

	missing := Oid("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	results, err := CollectReachable(store, []Oid{commit1.Oid()}, missing)
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, commit1.Oid(), tree1.Oid(), blob1.Oid())
}

// TestReachWithHaveZeroOid: have 含 ZeroOid → 跳过，等价于无 have
func TestReachWithHaveZeroOid(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-reach-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store := &LooseStore{Root: dir}

	blob1 := makeBlob("hello")
	tree1 := makeTree([]TreeEntry{
		{Mode: 0o100644, Name: "a.txt", Oid: blob1.Oid()},
	})
	commit1 := makeCommit(tree1.Oid(), nil, "initial\n")

	writeAll(t, store, blob1, tree1, commit1)

	results, err := CollectReachable(store, []Oid{commit1.Oid()}, ZeroOid)
	if err != nil {
		t.Fatalf("CollectReachable: %v", err)
	}
	assertOids(t, results, commit1.Oid(), tree1.Oid(), blob1.Oid())
}
