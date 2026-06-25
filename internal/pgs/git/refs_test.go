package git

import (
	"os"
	"path/filepath"
	"testing"
)

// 测试用 oid（合法 40hex）
var (
	oidA = Oid("a5ccb972673562ef5bad1a6cced799f9d71a796b")
	oidB = Oid("8ac2ce9315a60cf4ab8e50e7c5a3c9c1b2473653")
	oidC = Oid("04af7178a88c09441b737eec0cf0879c8571d22b")
	oidX = Oid("1111111111111111111111111111111111111111")
)

// writeRefFile 在 dir 下创建 name 文件（含父目录），内容为 content。
func writeRefFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o666); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// TestRefListSymrefHeadAndLoose: 空 HEAD symref + loose ref → List 正确
func TestRefListSymrefHeadAndLoose(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// 空 HEAD symref（master 不存在）
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/master\n")
	// 一个 loose ref
	writeRefFile(t, dir, "refs/heads/dev", string(oidA)+"\n")

	rs := NewRefStore(dir)
	refs, err := rs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("len(refs) = %d, want 2: %+v", len(refs), refs)
	}
	// 排序后 HEAD 在前（'H' < 'r'）
	if refs[0].Name != "HEAD" {
		t.Fatalf("refs[0].Name = %q, want HEAD", refs[0].Name)
	}
	if refs[0].Symref == nil || *refs[0].Symref != "refs/heads/master" {
		t.Fatalf("HEAD symref = %v, want refs/heads/master", refs[0].Symref)
	}
	if refs[0].Oid != ZeroOid {
		t.Fatalf("HEAD oid = %s, want ZeroOid (target missing)", refs[0].Oid)
	}
	if refs[1].Name != "refs/heads/dev" {
		t.Fatalf("refs[1].Name = %q, want refs/heads/dev", refs[1].Name)
	}
	if refs[1].Oid != oidA {
		t.Fatalf("dev oid = %s, want %s", refs[1].Oid, oidA)
	}
}

// TestRefListPackedAndLooseOverride: packed-refs + loose 同名 → loose 覆盖
func TestRefListPackedAndLooseOverride(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// packed-refs: master -> oidA, tag v1 -> oidC
	packed := string(oidA) + " refs/heads/master\n" + string(oidC) + " refs/tags/v1\n"
	if err := os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(packed), 0o666); err != nil {
		t.Fatal(err)
	}
	// loose 同名 master -> oidB（覆盖 packed 的 oidA）
	writeRefFile(t, dir, "refs/heads/master", string(oidB)+"\n")

	rs := NewRefStore(dir)
	refs, err := rs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	m := map[string]Oid{}
	for _, r := range refs {
		m[r.Name] = r.Oid
	}
	if m["refs/heads/master"] != oidB {
		t.Errorf("master oid = %s, want %s (loose override)", m["refs/heads/master"], oidB)
	}
	if m["refs/tags/v1"] != oidC {
		t.Errorf("v1 oid = %s, want %s (packed only)", m["refs/tags/v1"], oidC)
	}
}

// TestRefListEmptyRepo: 空仓库（无 refs 无 HEAD）→ 空切片不报错
func TestRefListEmptyRepo(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	rs := NewRefStore(dir)
	refs, err := rs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("len(refs) = %d, want 0", len(refs))
	}
}

// TestRefUpdateCreateNew: OldOid=Zero → 必须不存在，创建成功
func TestRefUpdateCreateNew(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	rs := NewRefStore(dir)
	results, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/new", OldOid: ZeroOid, NewOid: oidA},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(results) != 1 || !results[0].Ok {
		t.Fatalf("results = %+v, want 1 ok", results)
	}
	got, err := rs.Get("refs/heads/new")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != oidA {
		t.Fatalf("Get = %s, want %s", got, oidA)
	}
}

// TestRefUpdateCreateNested: 创建含斜杠的 nested ref
func TestRefUpdateCreateNested(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	rs := NewRefStore(dir)
	results, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/feature/x", OldOid: ZeroOid, NewOid: oidA},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !results[0].Ok {
		t.Fatalf("result not ok: %+v", results[0])
	}
	got, err := rs.Get("refs/heads/feature/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != oidA {
		t.Fatalf("Get = %s, want %s", got, oidA)
	}
}

// TestRefUpdateCASSuccess: OldOid 匹配现值 → 更新成功
func TestRefUpdateCASSuccess(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	writeRefFile(t, dir, "refs/heads/master", string(oidA)+"\n")
	rs := NewRefStore(dir)
	results, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: oidA, NewOid: oidB},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !results[0].Ok {
		t.Fatalf("result not ok: %+v", results[0])
	}
	got, err := rs.Get("refs/heads/master")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != oidB {
		t.Fatalf("Get = %s, want %s", got, oidB)
	}
}

// TestRefUpdateCASFailureIsolated: OldOid 不匹配 → 该 ref 失败，其他 ref 不受影响
func TestRefUpdateCASFailureIsolated(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	writeRefFile(t, dir, "refs/heads/master", string(oidA)+"\n")
	rs := NewRefStore(dir)
	results, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: oidX, NewOid: oidB}, // CAS 失败
		{Name: "refs/heads/dev", OldOid: ZeroOid, NewOid: oidC}, // 成功
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Ok {
		t.Errorf("master should fail: %+v", results[0])
	}
	if results[0].Reason == "" {
		t.Errorf("master should have reason")
	}
	if !results[1].Ok {
		t.Errorf("dev should succeed: %+v", results[1])
	}
	// master 未变
	got, err := rs.Get("refs/heads/master")
	if err != nil {
		t.Fatalf("Get master: %v", err)
	}
	if got != oidA {
		t.Errorf("master = %s, want %s (unchanged)", got, oidA)
	}
	// dev 已创建
	got, err = rs.Get("refs/heads/dev")
	if err != nil {
		t.Fatalf("Get dev: %v", err)
	}
	if got != oidC {
		t.Errorf("dev = %s, want %s", got, oidC)
	}
}

// TestRefUpdateCreateExists: OldOid=Zero 但 ref 已存在 → 失败
func TestRefUpdateCreateExists(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	writeRefFile(t, dir, "refs/heads/master", string(oidA)+"\n")
	rs := NewRefStore(dir)
	results, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: ZeroOid, NewOid: oidB},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if results[0].Ok {
		t.Fatalf("should fail when ref exists: %+v", results[0])
	}
	// 原值未变
	got, err := rs.Get("refs/heads/master")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != oidA {
		t.Fatalf("master = %s, want %s (unchanged)", got, oidA)
	}
}

// TestRefUpdateDelete: NewOid=Zero → 删除 ref
func TestRefUpdateDelete(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	writeRefFile(t, dir, "refs/heads/master", string(oidA)+"\n")
	rs := NewRefStore(dir)
	results, err := rs.Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: oidA, NewOid: ZeroOid},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !results[0].Ok {
		t.Fatalf("result not ok: %+v", results[0])
	}
	if _, err := rs.Get("refs/heads/master"); err == nil {
		t.Fatalf("Get master should fail after delete")
	}
	// 文件确实不存在
	if _, err := os.Stat(filepath.Join(dir, "refs/heads/master")); !os.IsNotExist(err) {
		t.Fatalf("ref file should be removed, stat err = %v", err)
	}
}

// TestRefGetDetachedHead: detached HEAD（直接 oid）→ Get("HEAD") 返回该 oid
func TestRefGetDetachedHead(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	writeRefFile(t, dir, "HEAD", string(oidA)+"\n")
	rs := NewRefStore(dir)
	got, err := rs.Get("HEAD")
	if err != nil {
		t.Fatalf("Get HEAD: %v", err)
	}
	if got != oidA {
		t.Fatalf("Get HEAD = %s, want %s", got, oidA)
	}
	// Head() detached 返回 ""
	head, err := rs.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head != "" {
		t.Fatalf("Head = %q, want empty (detached)", head)
	}
}

// TestRefGetSymrefHead: HEAD symref → Get("HEAD") 跟随到目标 ref
func TestRefGetSymrefHead(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	writeRefFile(t, dir, "HEAD", "ref: refs/heads/master\n")
	writeRefFile(t, dir, "refs/heads/master", string(oidA)+"\n")
	rs := NewRefStore(dir)
	got, err := rs.Get("HEAD")
	if err != nil {
		t.Fatalf("Get HEAD: %v", err)
	}
	if got != oidA {
		t.Fatalf("Get HEAD = %s, want %s", got, oidA)
	}
	head, err := rs.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head != "refs/heads/master" {
		t.Fatalf("Head = %q, want refs/heads/master", head)
	}
}

// TestRefGetPacked: Get 从 packed-refs 取 oid
func TestRefGetPacked(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	packed := "# pack-refs with: peeled fully-peeled \n" +
		string(oidA) + " refs/heads/master\n" +
		"^" + string(oidC) + "\n" + // peeled 行应被忽略
		string(oidB) + " refs/tags/v1\n"
	if err := os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(packed), 0o666); err != nil {
		t.Fatal(err)
	}
	rs := NewRefStore(dir)
	got, err := rs.Get("refs/heads/master")
	if err != nil {
		t.Fatalf("Get master: %v", err)
	}
	if got != oidA {
		t.Fatalf("Get master = %s, want %s", got, oidA)
	}
	got, err = rs.Get("refs/tags/v1")
	if err != nil {
		t.Fatalf("Get v1: %v", err)
	}
	if got != oidB {
		t.Fatalf("Get v1 = %s, want %s", got, oidB)
	}
	// peeled oid 不应作为独立 ref 出现
	if _, err := rs.Get("^" + string(oidC)); err == nil {
		t.Fatalf("peeled line should not be a ref")
	}
}

// TestRefGetNotFound: Get 不存在的 ref → 错误
func TestRefGetNotFound(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	rs := NewRefStore(dir)
	if _, err := rs.Get("refs/heads/nope"); err == nil {
		t.Fatalf("Get should fail for missing ref")
	}
}

// TestSetHead: 写入 HEAD symref，Head() 能正确读回
func TestSetHead(t *testing.T) {
	dir, err := os.MkdirTemp("", "pgit-refs-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	rs := NewRefStore(dir)
	if err := rs.SetHead("refs/heads/develop"); err != nil {
		t.Fatalf("SetHead: %v", err)
	}
	head, err := rs.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head != "refs/heads/develop" {
		t.Fatalf("Head = %q, want refs/heads/develop", head)
	}

	// 覆盖写：从 develop 切到 main
	if err := rs.SetHead("refs/heads/main"); err != nil {
		t.Fatalf("SetHead overwrite: %v", err)
	}
	head, err = rs.Head()
	if err != nil {
		t.Fatalf("Head after overwrite: %v", err)
	}
	if head != "refs/heads/main" {
		t.Fatalf("Head = %q, want refs/heads/main", head)
	}

	// HEAD.lock 不应残留
	if _, err := os.Stat(filepath.Join(dir, "HEAD.lock")); !os.IsNotExist(err) {
		t.Fatalf("HEAD.lock should not exist after SetHead, stat err = %v", err)
	}
}
