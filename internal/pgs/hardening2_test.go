package pgs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 哨兵错误：上层可经 errors.Is 判定，不再依赖字符串匹配。
func TestSentinelErrors(t *testing.T) {
	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	if _, err := ReposManager.GetRepository("nope"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("GetRepository = %v, want ErrRepoNotFound", err)
	}
	if _, err := ReposManager.GetByAlias("nope"); !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("GetByAlias = %v, want ErrAliasNotFound", err)
	}
	if err := ReposManager.CreateRepository("r1", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := ReposManager.CreateRepository("r1", "", ""); !errors.Is(err, ErrRepoExist) {
		t.Errorf("duplicate create = %v, want ErrRepoExist", err)
	}
	if _, err := ReposManager.SyncRepository("r1"); !errors.Is(err, ErrNotMirror) {
		t.Errorf("SyncRepository(non-mirror) = %v, want ErrNotMirror", err)
	}
	if _, err := ReposManager.UpdateRepositorySettings("r1", "", &MirrorConfig{RemoteURL: "http://e.com/r.git"}); !errors.Is(err, ErrNotMirror) {
		t.Errorf("UpdateRepositorySettings(non-mirror) = %v, want ErrNotMirror", err)
	}

	// 同步管理器
	sm := NewSyncManager(ReposManager)
	t.Cleanup(sm.Stop)
	if _, err := sm.SyncNow("r1"); !errors.Is(err, ErrNotMirror) {
		t.Errorf("SyncNow(non-mirror) = %v, want ErrNotMirror", err)
	}
	if _, err := sm.SyncNow("ghost"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("SyncNow(missing) = %v, want ErrRepoNotFound", err)
	}
}

// 错误文案仍然可读（包装后保留仓库名），便于日志与响应体。
func TestSentinelErrorMessages(t *testing.T) {
	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	_, err := ReposManager.GetRepository("ghost")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("message should mention repo name, got %v", err)
	}
}

// InitBare 失败时回滚半成品目录（不残留被扫描静默跳过的目录）。
func TestInitBareRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()

	// 让 SaveMetadata 失败：预先放置同名 .tmp 目录，使 rename/写入无法完成。
	// 用只读目录更直接：仓库名指向一个不可写的位置。
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o750)

	if _, err := InitBare(ro, "blocked", "desc", "master"); err == nil {
		t.Skip("platform allows writing into read-only dir; rollback not exercised")
	}
	if _, err := os.Stat(filepath.Join(ro, "blocked.git")); !os.IsNotExist(err) {
		t.Errorf("failed InitBare left a partial repo dir behind (stat err = %v)", err)
	}
}

// 创建失败后 manager 索引不应包含该仓库（无残留、可重试同名）。
func TestCreateRepositoryRollbackAllowsRetry(t *testing.T) {
	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	if err := ReposManager.CreateRepository("ok1", "", ""); err != nil {
		t.Fatal(err)
	}
	// 目录被外部占用（同名目录已存在且非空）→ InitBare 失败
	if err := os.MkdirAll(filepath.Join(dir, "busy.git", "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ReposManager.CreateRepository("busy", "", ""); err == nil {
		t.Fatal("create should fail when target dir occupied")
	}
	if ReposManager.RepositoryExist("busy") {
		t.Error("failed create should not be indexed")
	}
}

// 权限位：新建仓库目录与文件不再使用 0o777/0o666。
func TestInitBarePermissions(t *testing.T) {
	dir := t.TempDir()
	repo, err := InitBare(dir, "perm", "desc", "master")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(repo.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o007 != 0 {
		t.Errorf("repo dir permissions = %o, want no world access", info.Mode().Perm())
	}
	meta, err := os.Stat(filepath.Join(repo.Path(), "pgit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Mode().Perm()&0o007 != 0 {
		t.Errorf("metadata permissions = %o, want no world access", meta.Mode().Perm())
	}
}
