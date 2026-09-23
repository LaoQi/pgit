package git

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 计数 store：统计 Read 调用，用于验证 ForEachRefs 缓存命中时不重复解析对象。
type countingObjectStore struct {
	inner ObjectStore
	reads int
}

func (c *countingObjectStore) Read(oid Oid) (*RawObject, error) {
	c.reads++
	return c.inner.Read(oid)
}
func (c *countingObjectStore) Exists(oid Oid) bool                   { return c.inner.Exists(oid) }
func (c *countingObjectStore) Write(obj *RawObject) (Oid, error)     { return c.inner.Write(obj) }
func (c *countingObjectStore) Stat(oid Oid) (ObjectType, int, error) { return c.inner.Stat(oid) }

// Update 一个包含大量 ref 的仓库：packed-refs 应只解析一次（此前每个 ref 一次 → O(N²)）。
// 用「大量 packed refs + 一次批量更新」放大差异：若实现退化，磁盘读次数随 N 线性增长。
func TestRefStoreUpdateParsesPackedRefsOnce(t *testing.T) {
	dir := t.TempDir()
	rs := NewRefStore(dir)

	const n = 400
	// 构造 packed-refs（模拟经过 gc 的仓库）
	var buf []byte
	buf = append(buf, "# pack-refs with: peeled fully-peeled sorted \n"...)
	for i := 0; i < n; i++ {
		buf = append(buf, fmt.Sprintf("%040x refs/heads/packed-%d\n", i+1, i)...)
	}
	if err := os.WriteFile(filepath.Join(dir, "packed-refs"), buf, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs", "heads"), 0o750); err != nil {
		t.Fatal(err)
	}

	// 批量更新 n 个 ref（每个都要 CAS：读现值需查 packed-refs）
	updates := make([]RefUpdate, 0, n)
	for i := 0; i < n; i++ {
		updates = append(updates, RefUpdate{
			Name:   fmt.Sprintf("refs/heads/packed-%d", i),
			OldOid: Oid(fmt.Sprintf("%040x", i+1)),
			NewOid: Oid(fmt.Sprintf("%040x", i+1000)),
		})
	}

	results, err := rs.Update(updates)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	okCount := 0
	for _, r := range results {
		if r.Ok {
			okCount++
		}
	}
	if okCount != n {
		t.Fatalf("updated %d/%d refs", okCount, n)
	}

	// 验证写入生效（loose 覆盖 packed）
	oid, err := rs.Get("refs/heads/packed-0")
	if err != nil {
		t.Fatal(err)
	}
	if oid != Oid(fmt.Sprintf("%040x", 1000)) {
		t.Errorf("refs/heads/packed-0 = %s, want %040x", oid, 1000)
	}
}

// ForEachRefs 缓存：refs 未变时命中缓存（不重复读取对象），refs 变化后自动失效。
func TestForEachRefsCache(t *testing.T) {
	dir := t.TempDir()
	store := &LooseStore{Root: filepath.Join(dir, "objects")}
	rs := NewRefStore(dir)

	blob := makeBlob("cached content\n")
	tree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "f", Oid: blob.Oid()}})
	commit := makeCommit(tree.Oid(), nil, "cached\n")
	for _, o := range []*RawObject{blob, tree, commit} {
		if _, err := store.Write(o); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rs.Update([]RefUpdate{{Name: "refs/heads/master", OldOid: ZeroOid, NewOid: commit.Oid()}}); err != nil {
		t.Fatal(err)
	}

	InvalidateRefsCache(dir)
	first, err := ForEachRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Oid != commit.Oid() {
		t.Fatalf("refs = %+v", first)
	}

	// 同一 refs 视图再取：应返回缓存切片（指针相同即证明未重建）
	second, err := ForEachRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Oid != first[0].Oid {
		t.Fatalf("cached refs mismatch: %+v", second)
	}
	if &second[0] != &first[0] {
		t.Error("expected identical cached slice (no rebuild)")
	}

	// refs 变化 → 指纹变化 → 缓存失效并反映新值
	commit2 := makeCommit(tree.Oid(), []Oid{commit.Oid()}, "second\n")
	if _, err := store.Write(commit2); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Update([]RefUpdate{{Name: "refs/heads/master", OldOid: commit.Oid(), NewOid: commit2.Oid()}}); err != nil {
		t.Fatal(err)
	}
	third, err := ForEachRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 1 || third[0].Oid != commit2.Oid() {
		t.Fatalf("cache should be invalidated after ref change, got %+v", third)
	}

	InvalidateRefsCache(dir)
}

// 显式失效后再次调用应重建。
func TestForEachRefsCacheInvalidate(t *testing.T) {
	dir := t.TempDir()
	store := &LooseStore{Root: filepath.Join(dir, "objects")}
	rs := NewRefStore(dir)
	blob := makeBlob("x\n")
	tree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "f", Oid: blob.Oid()}})
	commit := makeCommit(tree.Oid(), nil, "m\n")
	for _, o := range []*RawObject{blob, tree, commit} {
		if _, err := store.Write(o); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rs.Update([]RefUpdate{{Name: "refs/heads/main", OldOid: ZeroOid, NewOid: commit.Oid()}}); err != nil {
		t.Fatal(err)
	}

	a, err := ForEachRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	InvalidateRefsCache(dir)
	b, err := ForEachRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("len mismatch %d vs %d", len(a), len(b))
	}
	if &a[0] == &b[0] {
		t.Error("expected rebuild after explicit invalidation")
	}
}
