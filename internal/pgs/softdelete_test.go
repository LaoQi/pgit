package pgs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 软删除：目录与 git 数据保留、只写 pgit.deleted 标记；索引注销后立即不可见；
// 同名重建被拒并给出可行动提示；重启扫描跳过标记仓库；删标记后重扫恢复。
func TestDeleteRepositorySoftDeletes(t *testing.T) {
	dir := newNamingManager(t)
	mustCreate(t, "r1")

	if err := ReposManager.DeleteRepository("r1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	marker := filepath.Join(dir, "r1.git", deletedMarkerFile)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "r1.git", "pgit.json")); err != nil {
		t.Fatalf("metadata must be kept on disk: %v", err)
	}
	if _, err := ReposManager.Resolve("r1"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("Resolve(r1) after delete = %v, want ErrRepoNotFound", err)
	}

	// 同名重建被拒：错误须提示软删除标记与出路，而不是裸 EEXIST。
	err := ReposManager.CreateRepository("r1", "", "master")
	if err == nil || !strings.Contains(err.Error(), "soft-deleted") {
		t.Fatalf("recreate same name: err = %v, want soft-deleted hint", err)
	}
	err = ReposManager.CreateMirrorRepository("r1", "", &MirrorConfig{RemoteURL: "http://example.com/x.git"})
	if err == nil || !strings.Contains(err.Error(), "soft-deleted") {
		t.Fatalf("recreate same name as mirror: err = %v, want soft-deleted hint", err)
	}

	// 模拟重启：扫描跳过标记仓库（不进索引、不占用 ref）。
	rescan := func() { InitReposManager(&RepositoriesManagerConfig{GitRoot: dir}) }
	rescan()
	if _, err := ReposManager.Resolve("r1"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("Resolve(r1) after rescan = %v, want ErrRepoNotFound", err)
	}
	// 标记仓库释放的 ref 可被其他仓库使用（跳过即不参与唯一性检测）。
	mustCreate(t, "other")

	// 删除标记文件后重扫：仓库恢复可见，数据完好。
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	rescan()
	repo, err := ReposManager.Resolve("r1")
	if err != nil {
		t.Fatalf("Resolve(r1) after marker removed: %v", err)
	}
	if repo.Name != "r1" || len(repo.Aliases) == 0 {
		t.Fatalf("restored repo = %+v", repo)
	}
}

// 目录已存在但无软删除标记（外部遗留目录）：报 already exists，同样不透传 EEXIST。
func TestCreateRepositoryRejectsExistingDir(t *testing.T) {
	dir := newNamingManager(t)
	if err := os.MkdirAll(filepath.Join(dir, "legacy.git", "objects"), 0o750); err != nil {
		t.Fatal(err)
	}
	err := ReposManager.CreateRepository("legacy", "", "master")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create over existing dir: err = %v, want already exists", err)
	}
}
