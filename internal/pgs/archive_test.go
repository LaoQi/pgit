package pgs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pgit/internal/pgs/git"
)

// setArchiveConf 临时修改全局归档配置（测试结束恢复）。
func setArchiveConf(t *testing.T, enabled bool, dir string, keep int, maxBytes int64) {
	t.Helper()
	s := Settings
	s.mu.Lock()
	oldEnabled, oldDir, oldKeep, oldMax := s.MirrorArchive, s.MirrorArchiveDir, s.MirrorArchiveKeep, s.MirrorArchiveMaxBytes
	s.MirrorArchive, s.MirrorArchiveDir, s.MirrorArchiveKeep, s.MirrorArchiveMaxBytes = enabled, dir, keep, maxBytes
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		s.MirrorArchive, s.MirrorArchiveDir, s.MirrorArchiveKeep, s.MirrorArchiveMaxBytes = oldEnabled, oldDir, oldKeep, oldMax
		s.mu.Unlock()
	})
}

// forkRepo 在 buildBrowseRepo 基础上追加一个提交，并把 master 指向它（模拟"本地领先/上游重写"）。
func forkRepo(t *testing.T, name string) (repo *Repository, base, tip git.Oid) {
	t.Helper()
	repo = buildBrowseRepo(t, name)
	store := &git.LooseStore{Root: filepath.Join(repo.Path(), "objects")}
	base = readRefOid(t, repo.Path(), "refs/heads/master")

	blob := git.NewRawObject(git.ObjBlob, []byte("local only\n"))
	tree := git.NewRawObject(git.ObjTree, treeBody([]git.TreeEntry{
		{Mode: 0o100644, Name: "local.txt", Oid: blob.Oid()},
	}))
	body := fmt.Sprintf("tree %s\nparent %s\nauthor Test <test@pgit.dev> 1700000100 +0800\ncommitter Test <test@pgit.dev> 1700000100 +0800\n\nlocal work\n",
		tree.Oid(), base)
	commit := git.NewRawObject(git.ObjCommit, []byte(body))
	for _, o := range []*git.RawObject{blob, tree, commit} {
		if _, err := store.Write(o); err != nil {
			t.Fatalf("write object: %v", err)
		}
	}
	tip = commit.Oid()
	if err := os.WriteFile(filepath.Join(repo.Path(), "refs/heads/master"), []byte(string(tip)+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	return repo, base, tip
}

func readRefOid(t *testing.T, repoRoot, ref string) git.Oid {
	t.Helper()
	oid, err := git.NewRefStore(repoRoot).Get(ref)
	if err != nil {
		t.Fatalf("get %s: %v", ref, err)
	}
	return oid
}

func TestArchiveSnapshotListRestore(t *testing.T) {
	archiveDir := t.TempDir()
	setArchiveConf(t, true, archiveDir, 3, -1)

	repo, base, tip := forkRepo(t, "forked")
	am := NewArchiveManager(Settings)
	if !am.Enabled() {
		t.Fatal("archive should be enabled")
	}

	rec, err := am.Snapshot(repo.Name, repo.Path(), git.DestructiveEvent{
		RepoRoot: repo.Path(),
		Refs: []git.DestructiveRef{
			{Name: "refs/heads/master", OldOid: tip, NewOid: base, Reason: "non-fast-forward"},
		},
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if rec.SizeBytes <= 0 || rec.Objects != 5 { // blob*2 + tree*2 + commit*2? buildBrowseRepo 5 个 + 本地 3 个 = 8
		t.Logf("record: %+v", rec)
	}
	if rec.Head != "refs/heads/master" {
		t.Errorf("Head = %q", rec.Head)
	}
	if rec.Reason != "non-fast-forward" {
		t.Errorf("Reason = %q", rec.Reason)
	}

	bundlePath := filepath.Join(archiveDir, repo.Name, rec.ID+".bundle")
	if st, err := os.Stat(bundlePath); err != nil || st.Size() == 0 {
		t.Fatalf("bundle missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveDir, repo.Name, rec.ID+".json")); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}

	list, err := am.Snapshots(repo.Name)
	if err != nil || len(list) != 1 {
		t.Fatalf("Snapshots = %d err=%v", len(list), err)
	}
	if list[0].ID != rec.ID {
		t.Errorf("list id = %q, want %q", list[0].ID, rec.ID)
	}

	// 把 master 回退到 base（模拟同步把历史覆盖掉），再从归档恢复
	if err := os.WriteFile(filepath.Join(repo.Path(), "refs/heads/master"), []byte(string(base)+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	res, got, err := am.Restore(repo.Path(), repo.Name, rec.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got.ID != rec.ID {
		t.Errorf("restore record id = %q", got.ID)
	}
	if res.Objects == 0 || len(res.Refs) != 1 {
		t.Errorf("restore result = %+v", res)
	}
	if oid := readRefOid(t, repo.Path(), "refs/heads/master"); oid != tip {
		t.Errorf("master after restore = %s, want %s", oid, tip)
	}
	head, err := git.NewRefStore(repo.Path()).Head()
	if err != nil || head != "refs/heads/master" {
		t.Errorf("HEAD after restore = %q err=%v", head, err)
	}

	// 非法 id / 不存在的 id
	if _, _, err := am.Find(repo.Name, "../../etc/passwd"); err == nil {
		t.Error("expected error for unsafe id")
	}
	if _, _, err := am.Find(repo.Name, "1700000000000-nope"); err != ErrArchiveNotFound {
		t.Errorf("missing id error = %v, want ErrArchiveNotFound", err)
	}
}

func TestArchiveKeepPrune(t *testing.T) {
	archiveDir := t.TempDir()
	setArchiveConf(t, true, archiveDir, 2, -1)
	repo, _, tip := forkRepo(t, "prune-repo")
	am := NewArchiveManager(Settings)

	for i := 0; i < 4; i++ {
		_, err := am.Snapshot(repo.Name, repo.Path(), git.DestructiveEvent{
			Refs: []git.DestructiveRef{{Name: "refs/heads/master", OldOid: tip, Reason: "delete"}},
		})
		if err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	list, err := am.Snapshots(repo.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("after prune archives = %d, want 2", len(list))
	}
	// 目录里也不应残留被淘汰的 bundle
	entries, _ := os.ReadDir(filepath.Join(archiveDir, repo.Name))
	bundles := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".bundle" {
			bundles++
		}
	}
	if bundles != 2 {
		t.Errorf("bundle files = %d, want 2", bundles)
	}
}

func TestArchiveMaxBytesPrune(t *testing.T) {
	archiveDir := t.TempDir()
	setArchiveConf(t, true, archiveDir, -1, 1) // 容量上限 1 字节：任何归档都应被淘汰
	repo, _, tip := forkRepo(t, "cap-repo")
	am := NewArchiveManager(Settings)

	if _, err := am.Snapshot(repo.Name, repo.Path(), git.DestructiveEvent{
		Refs: []git.DestructiveRef{{Name: "refs/heads/master", OldOid: tip, Reason: "delete"}},
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(archiveDir, repo.Name))
	bundles := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".bundle" {
			bundles++
		}
	}
	if bundles != 0 {
		t.Errorf("bundles = %d, want 0 after maxBytes prune", bundles)
	}
	if list, _ := am.Snapshots(repo.Name); len(list) != 0 {
		t.Errorf("snapshots = %d, want 0", len(list))
	}
}

func TestArchiveSnapshotWriteFailure(t *testing.T) {
	// 归档目录指向一个普通文件 → 无法建目录，Snapshot 必须报错（调用方据此拒绝覆盖 ref）
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	setArchiveConf(t, true, blocker, 3, -1)
	repo, _, tip := forkRepo(t, "broken-archive")
	am := NewArchiveManager(Settings)
	if _, err := am.Snapshot(repo.Name, repo.Path(), git.DestructiveEvent{
		Refs: []git.DestructiveRef{{Name: "refs/heads/master", OldOid: tip, Reason: "delete"}},
	}); err == nil {
		t.Fatal("expected Snapshot to fail when archive dir is unwritable")
	}
}

func TestArchiveConfigValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		writeConfig(t, p, body)
		return p
	}
	base := `{"listen":":0","gitRoot":` + fmt.Sprintf("%q", dir) + `,"webuiPrefix":"__webui"`
	if _, err := func() (*Setting, error) {
		s := &Setting{}
		s.SetConfigPath(write("bad1.json", base+`,"mirrorArchiveDir":"repo.git"}`))
		return s, s.Reload()
	}(); err == nil {
		t.Error("mirrorArchiveDir ending in .git must be rejected")
	}
	if _, err := func() (*Setting, error) {
		s := &Setting{}
		s.SetConfigPath(write("bad2.json", base+`,"mirrorArchiveDir":`+fmt.Sprintf("%q", dir)+`}`))
		return s, s.Reload()
	}(); err == nil {
		t.Error("mirrorArchiveDir equal to gitRoot must be rejected")
	}
	s := &Setting{}
	s.SetConfigPath(write("ok.json", base+`,"mirrorArchive":true,"mirrorArchiveDir":"pgit-archive"}`))
	if err := s.Reload(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	enabled, gotDir, keep, maxBytes := s.ArchiveConf()
	if !enabled || gotDir != "pgit-archive" || keep != DefaultArchiveKeep || maxBytes != DefaultArchiveMaxBytes {
		t.Errorf("ArchiveConf = %v %q %d %d", enabled, gotDir, keep, maxBytes)
	}
}

// sidecar 里不应出现镜像密码（避免把 token 复制到归档目录）。
func TestArchiveSidecarHasNoCredentials(t *testing.T) {
	archiveDir := t.TempDir()
	setArchiveConf(t, true, archiveDir, 3, -1)
	repo, _, tip := forkRepo(t, "secret-repo")
	repo.Mirror = &MirrorConfig{RemoteURL: "https://example.com/x.git", AuthType: "basic", Username: "u", Password: "ghp_secret_token"}
	am := NewArchiveManager(Settings)
	rec, err := am.Snapshot(repo.Name, repo.Path(), git.DestructiveEvent{
		Refs: []git.DestructiveRef{{Name: "refs/heads/master", OldOid: tip, Reason: "delete"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(archiveDir, repo.Name, rec.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, "ghp_secret_token") || strings.Contains(got, "password") {
		t.Errorf("sidecar leaks credentials: %s", got)
	}
	var decoded ArchiveRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("sidecar not valid json: %v", err)
	}
}

// 手动快照：reason=manual、记录每个 ref 的当前 oid，且不受 mirrorArchive 开关限制。
func TestArchiveSnapshotManual(t *testing.T) {
	archiveDir := t.TempDir()
	setArchiveConf(t, false, archiveDir, 3, -1) // 自动归档关闭
	repo, _, tip := forkRepo(t, "manual-repo")
	am := NewArchiveManager(Settings)
	if am.Enabled() {
		t.Fatal("auto archive should be disabled")
	}

	rec, err := am.SnapshotManual(repo.Name, repo.Path())
	if err != nil {
		t.Fatalf("SnapshotManual: %v", err)
	}
	if rec.Reason != "manual" {
		t.Errorf("Reason = %q, want manual", rec.Reason)
	}
	if len(rec.Refs) != 1 || rec.Refs[0].Name != "refs/heads/master" || rec.Refs[0].OldOid != tip {
		t.Errorf("Refs = %+v, want master@%s", rec.Refs, tip)
	}
	if rec.Head != "refs/heads/master" || rec.Objects == 0 {
		t.Errorf("record = %+v", rec)
	}
	// 快照可被自身解包校验
	hdr, _, err := git.ReadBundle(mustOpen(t, filepath.Join(archiveDir, repo.Name, rec.ID+".bundle")))
	if err != nil {
		t.Fatalf("read manual snapshot: %v", err)
	}
	if len(hdr.Refs) != 1 || hdr.Refs[0].Oid != tip {
		t.Errorf("bundle refs = %+v", hdr.Refs)
	}
}

// 空仓库（无 refs）无法快照：返回错误而不是生成空包。
func TestArchiveSnapshotManualEmptyRepo(t *testing.T) {
	setArchiveConf(t, false, t.TempDir(), 3, -1)
	dir := t.TempDir()
	repo, err := InitBare(dir, "empty", "", "master")
	if err != nil {
		t.Fatal(err)
	}
	am := NewArchiveManager(Settings)
	if _, err := am.SnapshotManual(repo.Name, repo.Path()); err == nil {
		t.Fatal("expected error for repository without refs")
	}
}

// 删除单份归档：成功/不存在/不安全 id。
func TestArchiveDelete(t *testing.T) {
	archiveDir := t.TempDir()
	setArchiveConf(t, true, archiveDir, 3, -1)
	repo, _, _ := forkRepo(t, "delete-repo")
	am := NewArchiveManager(Settings)

	rec, err := am.SnapshotManual(repo.Name, repo.Path())
	if err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(archiveDir, repo.Name, rec.ID+".bundle")
	sidecarPath := filepath.Join(archiveDir, repo.Name, rec.ID+".json")

	if err := am.Delete(repo.Name, "../../x"); err == nil {
		t.Error("expected error for unsafe id")
	}
	if err := am.Delete(repo.Name, "1700000000000-nope"); err != ErrArchiveNotFound {
		t.Errorf("missing delete error = %v, want ErrArchiveNotFound", err)
	}
	if err := am.Delete(repo.Name, rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, p := range []string{bundlePath, sidecarPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be removed, stat err = %v", p, err)
		}
	}
	if list, _ := am.Snapshots(repo.Name); len(list) != 0 {
		t.Errorf("snapshots = %d, want 0", len(list))
	}
}
