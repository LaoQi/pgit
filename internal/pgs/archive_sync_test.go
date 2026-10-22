package pgs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgit/internal/pgs/git"
)

// gitUpstream 起一个只读 smart-http 上游（只用 upload-pack），复用 git 包的协议入口。
func gitUpstream(t *testing.T, gitRoot string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
		out, err := git.ServeInfoRefs(gitRoot, service)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(out)
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_ = git.HandleUploadPack(gitRoot, r.Body, w)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// writeUpstreamCommit 在上游裸仓库追加一个提交并移动 master。
func writeUpstreamCommit(t *testing.T, repoRoot string, parent git.Oid, msg string) git.Oid {
	t.Helper()
	store := &git.LooseStore{Root: filepath.Join(repoRoot, "objects")}
	blob := git.NewRawObject(git.ObjBlob, []byte(msg+"\n"))
	tree := git.NewRawObject(git.ObjTree, treeBody([]git.TreeEntry{
		{Mode: 0o100644, Name: "f.txt", Oid: blob.Oid()},
	}))
	body := fmt.Sprintf("tree %s\n", tree.Oid())
	if parent != "" && !parent.IsZero() {
		body += fmt.Sprintf("parent %s\n", parent)
	}
	body += fmt.Sprintf("author Test <test@pgit.dev> 1700000000 +0800\ncommitter Test <test@pgit.dev> 1700000000 +0800\n\n%s\n", msg)
	commit := git.NewRawObject(git.ObjCommit, []byte(body))
	for _, o := range []*git.RawObject{blob, tree, commit} {
		if _, err := store.Write(o); err != nil {
			t.Fatalf("write object: %v", err)
		}
	}
	rs := git.NewRefStore(repoRoot)
	old := git.ZeroOid
	if cur, err := rs.Get("refs/heads/master"); err == nil {
		old = cur
	}
	if _, err := rs.Update([]git.RefUpdate{{Name: "refs/heads/master", OldOid: old, NewOid: commit.Oid()}}); err != nil {
		t.Fatalf("update upstream ref: %v", err)
	}
	return commit.Oid()
}

// TestSyncArchivesRewrittenHistory 验证镜像同步在非快进覆盖前自动归档，并照常更新 ref。
func TestSyncArchivesRewrittenHistory(t *testing.T) {
	archiveDir := t.TempDir()
	setArchiveConf(t, true, archiveDir, 3, -1)

	upstreamRoot := t.TempDir()
	if _, err := InitBare(upstreamRoot, "repo", "upstream", "master"); err != nil {
		t.Fatal(err)
	}
	upRepoRoot := filepath.Join(upstreamRoot, "repo.git")
	base := writeUpstreamCommit(t, upRepoRoot, git.ZeroOid, "base")
	ts := gitUpstream(t, upRepoRoot)

	gitRoot := t.TempDir()
	InitReposManager(&RepositoriesManagerConfig{GitRoot: gitRoot})
	mgr := ReposManager
	if err := mgr.CreateMirrorRepository("mirror-archive", "", &MirrorConfig{
		RemoteURL: ts.URL + "/repo.git",
		AuthType:  "none",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.SyncRepository("mirror-archive"); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	repo, _ := mgr.GetRepository("mirror-archive")
	if oid := readRefOid(t, repo.Path(), "refs/heads/master"); oid != base {
		t.Fatalf("master = %s, want %s", oid, base)
	}

	// 上游历史被重写（同一父提交上的新提交）
	// 与 base 无共同祖先 = 真正的历史替换（非快进）
	rewritten := writeUpstreamCommit(t, upRepoRoot, git.ZeroOid, "rewritten upstream")
	result, err := mgr.SyncRepository("mirror-archive")
	if err != nil {
		t.Fatalf("sync after rewrite: %v", err)
	}
	if len(result.Destructive) != 1 || result.ArchiveError != "" {
		t.Fatalf("destructive = %+v archiveErr = %q", result.Destructive, result.ArchiveError)
	}
	if oid := readRefOid(t, repo.Path(), "refs/heads/master"); oid != rewritten {
		t.Errorf("master = %s, want rewritten %s", oid, rewritten)
	}
	am := mgr.Archive()
	list, err := am.Snapshots("mirror-archive")
	if err != nil || len(list) != 1 {
		t.Fatalf("archives = %d err=%v", len(list), err)
	}
	if list[0].Refs[0].OldOid != base {
		t.Errorf("archived old oid = %s, want %s", list[0].Refs[0].OldOid, base)
	}
	bundle := filepath.Join(archiveDir, "mirror-archive", list[0].ID+".bundle")
	if st, err := os.Stat(bundle); err != nil || st.Size() == 0 {
		t.Fatalf("bundle missing: %v", err)
	}
	// 归档包能被 pgit 自身解包（恢复路径可用）
	hdr, _, err := git.ReadBundle(mustOpen(t, bundle))
	if err != nil {
		t.Fatalf("read archived bundle: %v", err)
	}
	if len(hdr.Refs) != 1 || hdr.Refs[0].Oid != base {
		t.Errorf("bundle refs = %+v, want master@%s", hdr.Refs, base)
	}
}

// TestSyncArchiveFailureKeepsRef 验证归档失败时破坏性 ref 不被覆盖，且 lastError 记录原因。
func TestSyncArchiveFailureKeepsRef(t *testing.T) {
	upstreamRoot := t.TempDir()
	if _, err := InitBare(upstreamRoot, "repo", "upstream", "master"); err != nil {
		t.Fatal(err)
	}
	upRepoRoot := filepath.Join(upstreamRoot, "repo.git")
	base := writeUpstreamCommit(t, upRepoRoot, git.ZeroOid, "base")
	ts := gitUpstream(t, upRepoRoot)

	gitRoot := t.TempDir()
	InitReposManager(&RepositoriesManagerConfig{GitRoot: gitRoot})
	mgr := ReposManager
	if err := mgr.CreateMirrorRepository("mirror-nofail", "", &MirrorConfig{
		RemoteURL: ts.URL + "/repo.git",
		AuthType:  "none",
	}); err != nil {
		t.Fatal(err)
	}

	// 归档目录先合法，完成首次同步
	archiveDir := t.TempDir()
	setArchiveConf(t, true, archiveDir, 3, -1)
	if _, err := mgr.SyncRepository("mirror-nofail"); err != nil {
		t.Fatal(err)
	}

	// 让归档目录不可用（指向普通文件），上游再重写历史 → 同步成功但 ref 保持旧值
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	setArchiveConf(t, true, blocker, 3, -1)
	rewritten := writeUpstreamCommit(t, upRepoRoot, git.ZeroOid, "rewritten again")
	res, err := mgr.SyncRepository("mirror-nofail")
	if err != nil {
		t.Fatalf("sync should not fail hard: %v", err)
	}
	if len(res.DestructiveSkipped) != 1 || res.ArchiveError == "" {
		t.Fatalf("skipped = %+v archiveErr = %q", res.DestructiveSkipped, res.ArchiveError)
	}
	repo, _ := mgr.GetRepository("mirror-nofail")
	if oid := readRefOid(t, repo.Path(), "refs/heads/master"); oid != base {
		t.Errorf("master = %s, want unchanged %s", oid, base)
	}
	if !strings.Contains(repo.Mirror.LastError, "archive failed") {
		t.Errorf("lastError = %q, want archive failure", repo.Mirror.LastError)
	}
	_ = rewritten
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
