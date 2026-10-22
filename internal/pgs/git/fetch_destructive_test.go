package git

import (
	"errors"
	"path/filepath"
	"testing"
)

// divergentSetup 构造「上游与本地分叉」的镜像场景：
// 上游 base → upstreamTip，本地 base → localTip（本地多一个提交，上游历史被重写）。
func divergentSetup(t *testing.T) (remoteDir, localRoot string, base, localTip, upstreamTip Oid) {
	t.Helper()
	remoteDir, base = makeRepoWithCommit(t)
	ts := newFetchTestServer(t, remoteDir)
	localRoot = makeEmptyLocalRepo(t)
	if _, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil); err != nil {
		t.Fatalf("initial fetch: %v", err)
	}

	// 本地分叉提交（只在本地存在）
	localBlob := makeBlob("local only\n")
	localTree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "local.txt", Oid: localBlob.Oid()}})
	localCommit := makeCommit(localTree.Oid(), []Oid{base}, "local work\n")
	localStore := &LooseStore{Root: filepath.Join(localRoot, "objects")}
	writeAll(t, localStore, localBlob, localTree, localCommit)
	if _, err := NewRefStore(localRoot).Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: base, NewOid: localCommit.Oid()},
	}); err != nil {
		t.Fatalf("update local ref: %v", err)
	}

	// 上游同一父提交上的另一个提交（模拟 force push 后的历史）
	upBlob := makeBlob("upstream rewritten\n")
	upTree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "up.txt", Oid: upBlob.Oid()}})
	upCommit := makeCommit(upTree.Oid(), []Oid{base}, "upstream rewritten\n")
	remoteStore := &LooseStore{Root: filepath.Join(remoteDir, "objects")}
	writeAll(t, remoteStore, upBlob, upTree, upCommit)
	if _, err := NewRefStore(remoteDir).Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: base, NewOid: upCommit.Oid()},
	}); err != nil {
		t.Fatalf("update remote ref: %v", err)
	}
	return remoteDir, localRoot, base, localCommit.Oid(), upCommit.Oid()
}

func TestFetchDestructiveNonFastForwardArchived(t *testing.T) {
	remoteDir, localRoot, base, localTip, upTip := divergentSetup(t)
	ts := newFetchTestServer(t, remoteDir)

	var seen []DestructiveEvent
	opts := FetchOptions{OnDestructive: func(ev DestructiveEvent) error {
		seen = append(seen, ev)
		return nil
	}}
	res, err := FetchRemoteWithOptions(ts.URL+"/repo.git", localRoot, nil, opts)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("OnDestructive calls = %d, want 1", len(seen))
	}
	ev := seen[0]
	if ev.RepoRoot != localRoot {
		t.Errorf("RepoRoot = %q", ev.RepoRoot)
	}
	if len(ev.Refs) != 1 || ev.Refs[0].Name != "refs/heads/master" {
		t.Fatalf("event refs = %+v", ev.Refs)
	}
	got := ev.Refs[0]
	if got.Reason != "non-fast-forward" || got.OldOid != localTip || got.NewOid != upTip {
		t.Errorf("event ref = %+v, want fork %s→%s", got, localTip, upTip)
	}
	// 回调完成后照常覆盖本地（归档已成功）
	if len(res.Destructive) != 1 || res.ArchiveError != "" {
		t.Errorf("result destructive = %+v archiveErr = %q", res.Destructive, res.ArchiveError)
	}
	assertRefEquals(t, localRoot, "refs/heads/master", upTip)
	// 本地领先的历史对象仍在磁盘（无 GC）
	assertObjectExists(t, localRoot, localTip)
	_ = base
}

func TestFetchDestructiveArchiveFailureHoldsRef(t *testing.T) {
	remoteDir, localRoot, _, localTip, _ := divergentSetup(t)
	ts := newFetchTestServer(t, remoteDir)

	boom := errors.New("disk full")
	opts := FetchOptions{OnDestructive: func(DestructiveEvent) error { return boom }}
	res, err := FetchRemoteWithOptions(ts.URL+"/repo.git", localRoot, nil, opts)
	if err != nil {
		t.Fatalf("fetch should succeed even if archive fails: %v", err)
	}
	if len(res.DestructiveSkipped) != 1 {
		t.Fatalf("DestructiveSkipped = %+v", res.DestructiveSkipped)
	}
	if res.ArchiveError != boom.Error() {
		t.Errorf("ArchiveError = %q", res.ArchiveError)
	}
	// fail-safe：本地 ref 保持原值（旧历史仍可访问）
	assertRefEquals(t, localRoot, "refs/heads/master", localTip)
}

func TestFetchDestructiveRefDeletion(t *testing.T) {
	remoteDir, baseOid := makeRepoWithCommit(t)
	// 上游再加一个分支
	store := &LooseStore{Root: filepath.Join(remoteDir, "objects")}
	blob := makeBlob("gone\n")
	tree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "gone.txt", Oid: blob.Oid()}})
	goneCommit := makeCommit(tree.Oid(), []Oid{baseOid}, "gone\n")
	writeAll(t, store, blob, tree, goneCommit)
	if _, err := NewRefStore(remoteDir).Update([]RefUpdate{
		{Name: "refs/heads/gone", OldOid: ZeroOid, NewOid: goneCommit.Oid()},
	}); err != nil {
		t.Fatal(err)
	}

	ts := newFetchTestServer(t, remoteDir)
	localRoot := makeEmptyLocalRepo(t)
	if _, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil); err != nil {
		t.Fatalf("initial fetch: %v", err)
	}
	assertRefEquals(t, localRoot, "refs/heads/gone", goneCommit.Oid())

	// 上游删除分支 + master 快进（保证本次有对象传输）
	if _, err := NewRefStore(remoteDir).Update([]RefUpdate{
		{Name: "refs/heads/gone", OldOid: goneCommit.Oid(), NewOid: ZeroOid},
	}); err != nil {
		t.Fatal(err)
	}
	newBlob := makeBlob("after\n")
	newTree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "after.txt", Oid: newBlob.Oid()}})
	newCommit := makeCommit(newTree.Oid(), []Oid{baseOid}, "after\n")
	writeAll(t, store, newBlob, newTree, newCommit)
	if _, err := NewRefStore(remoteDir).Update([]RefUpdate{
		{Name: "refs/heads/master", OldOid: baseOid, NewOid: newCommit.Oid()},
	}); err != nil {
		t.Fatal(err)
	}

	var seen []DestructiveRef
	opts := FetchOptions{OnDestructive: func(ev DestructiveEvent) error {
		seen = append(seen, ev.Refs...)
		return nil
	}}
	res, err := FetchRemoteWithOptions(ts.URL+"/repo.git", localRoot, nil, opts)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(seen) != 1 || seen[0].Name != "refs/heads/gone" || seen[0].Reason != "delete" {
		t.Fatalf("destructive refs = %+v", seen)
	}
	if _, err := NewRefStore(localRoot).Get("refs/heads/gone"); err == nil {
		t.Error("refs/heads/gone should be deleted after successful archive")
	}
	assertRefEquals(t, localRoot, "refs/heads/master", newCommit.Oid())
	if res.RefsDeleted != 1 {
		t.Errorf("RefsDeleted = %d, want 1", res.RefsDeleted)
	}
}

func TestFetchDestructiveDeletionFailureHoldsRef(t *testing.T) {
	remoteDir, baseOid := makeRepoWithCommit(t)
	store := &LooseStore{Root: filepath.Join(remoteDir, "objects")}
	blob := makeBlob("gone\n")
	tree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "gone.txt", Oid: blob.Oid()}})
	goneCommit := makeCommit(tree.Oid(), []Oid{baseOid}, "gone\n")
	writeAll(t, store, blob, tree, goneCommit)
	rs := NewRefStore(remoteDir)
	rs.Update([]RefUpdate{{Name: "refs/heads/gone", OldOid: ZeroOid, NewOid: goneCommit.Oid()}})

	ts := newFetchTestServer(t, remoteDir)
	localRoot := makeEmptyLocalRepo(t)
	if _, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil); err != nil {
		t.Fatal(err)
	}
	rs.Update([]RefUpdate{{Name: "refs/heads/gone", OldOid: goneCommit.Oid(), NewOid: ZeroOid}})
	newBlob := makeBlob("after\n")
	newTree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "after.txt", Oid: newBlob.Oid()}})
	newCommit := makeCommit(newTree.Oid(), []Oid{baseOid}, "after\n")
	writeAll(t, store, newBlob, newTree, newCommit)
	rs.Update([]RefUpdate{{Name: "refs/heads/master", OldOid: baseOid, NewOid: newCommit.Oid()}})

	opts := FetchOptions{OnDestructive: func(DestructiveEvent) error { return errors.New("no space") }}
	res, err := FetchRemoteWithOptions(ts.URL+"/repo.git", localRoot, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.DestructiveSkipped) != 1 {
		t.Fatalf("skipped = %+v", res.DestructiveSkipped)
	}
	// 被归档拒绝的删除不执行；其余 ref 照常快进
	assertRefEquals(t, localRoot, "refs/heads/gone", goneCommit.Oid())
	assertRefEquals(t, localRoot, "refs/heads/master", newCommit.Oid())
}

func TestFetchFastForwardDoesNotTriggerArchive(t *testing.T) {
	remoteDir, commit1 := makeRepoWithCommit(t)
	ts := newFetchTestServer(t, remoteDir)
	localRoot := makeEmptyLocalRepo(t)
	if _, err := FetchRemote(ts.URL+"/repo.git", localRoot, nil); err != nil {
		t.Fatal(err)
	}
	// 上游快进
	store := &LooseStore{Root: filepath.Join(remoteDir, "objects")}
	blob := makeBlob("ff\n")
	tree := makeTree([]TreeEntry{{Mode: 0o100644, Name: "ff.txt", Oid: blob.Oid()}})
	commit2 := makeCommit(tree.Oid(), []Oid{commit1}, "fast forward\n")
	writeAll(t, store, blob, tree, commit2)
	NewRefStore(remoteDir).Update([]RefUpdate{{Name: "refs/heads/master", OldOid: commit1, NewOid: commit2.Oid()}})

	called := false
	opts := FetchOptions{OnDestructive: func(DestructiveEvent) error { called = true; return nil }}
	if _, err := FetchRemoteWithOptions(ts.URL+"/repo.git", localRoot, nil, opts); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("OnDestructive must not fire for fast-forward updates")
	}
}
