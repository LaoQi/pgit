package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgit/internal/pgs"
	"pgit/internal/pgs/git"
)

// setGlobalArchive 直接设置全局 Settings 的归档配置（handler 与 manager 共用同一个 Settings 指针），
// 测试结束恢复原值。调用方保证测试串行（同包测试默认串行）。
func setGlobalArchive(t *testing.T, dir string) {
	t.Helper()
	s := pgs.Settings
	oldEnabled, oldDir, oldKeep, oldMax := s.MirrorArchive, s.MirrorArchiveDir, s.MirrorArchiveKeep, s.MirrorArchiveMaxBytes
	oldPrefix := s.WebUIPrefix
	s.MirrorArchive = true
	s.MirrorArchiveDir = dir
	s.MirrorArchiveKeep = 3
	s.MirrorArchiveMaxBytes = -1
	if s.WebUIPrefix == "" {
		s.WebUIPrefix = "__webui"
	}
	t.Cleanup(func() {
		s.MirrorArchive, s.MirrorArchiveDir, s.MirrorArchiveKeep, s.MirrorArchiveMaxBytes = oldEnabled, oldDir, oldKeep, oldMax
		s.WebUIPrefix = oldPrefix
	})
}

func TestArchiveAPIListAndRestore(t *testing.T) {
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	archiveDir := t.TempDir()
	setGlobalArchive(t, archiveDir)
	h := NewHTTPHandler(pgs.ReposManager, pgs.Settings, nil, nil)

	seedRepo(t, "demo", "team/demo")
	repo, err := pgs.ReposManager.GetRepository("demo")
	if err != nil {
		t.Fatal(err)
	}
	masterOid, err := git.NewRefStore(repo.Path()).Get("refs/heads/master")
	if err != nil {
		t.Fatal(err)
	}

	// 空列表
	rec := do(h, http.MethodGet, "/api/v1/repos/archives?ref=demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"total":0`) || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Errorf("empty list body = %s", rec.Body.String())
	}

	// 生成一个归档（模拟同步前的快照）
	snap, err := pgs.ReposManager.Archive().Snapshot(repo.Name, repo.Path(), git.DestructiveEvent{
		Refs: []git.DestructiveRef{{Name: "refs/heads/master", OldOid: masterOid, Reason: "delete"}},
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	rec = do(h, http.MethodGet, "/api/v1/repos/archives?ref=team/demo")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), snap.ID) {
		t.Fatalf("list by alias = %d (%s)", rec.Code, rec.Body.String())
	}

	// 恢复：先删掉 ref 与 HEAD（模拟仓库被覆盖/删除 ref）
	if err := os.Remove(filepath.Join(repo.Path(), "refs/heads/master")); err != nil {
		t.Fatal(err)
	}
	// confirm 错误 → 400
	if rec := do(h, http.MethodPost, "/api/v1/repos/archives/restore?ref=demo&archive="+snap.ID+"&confirm=wrong"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad confirm = %d, want 400", rec.Code)
	}
	// 缺 archive → 400
	if rec := do(h, http.MethodPost, "/api/v1/repos/archives/restore?ref=demo&confirm=demo"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing archive = %d, want 400", rec.Code)
	}
	// 不存在的 id → 404
	if rec := do(h, http.MethodPost, "/api/v1/repos/archives/restore?ref=demo&archive=1700000000000-nope&confirm=demo"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown archive = %d, want 404", rec.Code)
	}
	// 路径穿越 id → 400
	if rec := do(h, http.MethodPost, "/api/v1/repos/archives/restore?ref=demo&archive=../../etc/passwd&confirm=demo"); rec.Code != http.StatusBadRequest {
		t.Errorf("unsafe archive id = %d, want 400", rec.Code)
	}

	rec = do(h, http.MethodPost, "/api/v1/repos/archives/restore?ref=team/demo&archive="+snap.ID+"&confirm=demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("restore body = %s", rec.Body.String())
	}
	got, err := git.NewRefStore(repo.Path()).Get("refs/heads/master")
	if err != nil || got != masterOid {
		t.Errorf("master after restore = %s err=%v, want %s", got, err, masterOid)
	}
	head, err := git.NewRefStore(repo.Path()).Head()
	if err != nil || head != "refs/heads/master" {
		t.Errorf("HEAD after restore = %q err=%v", head, err)
	}
}

func TestArchiveAPIUnknownRepo(t *testing.T) {
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	setGlobalArchive(t, t.TempDir())
	h := NewHTTPHandler(pgs.ReposManager, pgs.Settings, nil, nil)

	if rec := do(h, http.MethodGet, "/api/v1/repos/archives?ref=nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown repo = %d, want 404", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/repos/archives"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing ref = %d, want 400", rec.Code)
	}
}

// 手动快照 / 下载 / 删除三个管理端点。
func TestArchiveAPIManage(t *testing.T) {
	dir := t.TempDir()
	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { pgs.ReposManager = nil })
	archiveDir := t.TempDir()
	setGlobalArchive(t, archiveDir)
	h := NewHTTPHandler(pgs.ReposManager, pgs.Settings, nil, nil)

	seedRepo(t, "manage", "team/manage")
	repo, err := pgs.ReposManager.GetRepository("manage")
	if err != nil {
		t.Fatal(err)
	}

	// 手动快照（用别名寻址）
	rec := do(h, http.MethodPost, "/api/v1/repos/archives?ref=team/manage")
	if rec.Code != http.StatusOK {
		t.Fatalf("create snapshot = %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		OK      bool              `json:"ok"`
		Archive pgs.ArchiveRecord `json:"archive"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create snapshot body: %v (%s)", err, rec.Body.String())
	}
	if !created.OK || created.Archive.Reason != "manual" || created.Archive.ID == "" {
		t.Fatalf("created archive = %+v", created.Archive)
	}

	// 下载 bundle
	dl := do(h, http.MethodGet, "/api/v1/repos/archives/bundle?ref=manage&archive="+created.Archive.ID)
	if dl.Code != http.StatusOK {
		t.Fatalf("download = %d (%s)", dl.Code, dl.Body.String())
	}
	if ct := dl.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("download Content-Type = %q", ct)
	}
	if cd := dl.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".bundle") {
		t.Errorf("download Content-Disposition = %q", cd)
	}
	if !strings.HasPrefix(dl.Body.String(), "# v2 git bundle") {
		t.Errorf("download body is not a bundle: %q", dl.Body.String()[:40])
	}
	// 缺 archive / 未知 id
	if r := do(h, http.MethodGet, "/api/v1/repos/archives/bundle?ref=manage"); r.Code != http.StatusBadRequest {
		t.Errorf("download without archive = %d, want 400", r.Code)
	}
	if r := do(h, http.MethodGet, "/api/v1/repos/archives/bundle?ref=manage&archive=1700000000000-nope"); r.Code != http.StatusNotFound {
		t.Errorf("download unknown archive = %d, want 404", r.Code)
	}

	// 删除：confirm 必须匹配；成功后列表为空
	if r := do(h, http.MethodDelete, "/api/v1/repos/archives?ref=manage&archive="+created.Archive.ID+"&confirm=wrong"); r.Code != http.StatusBadRequest {
		t.Errorf("delete confirm mismatch = %d, want 400", r.Code)
	}
	if r := do(h, http.MethodDelete, "/api/v1/repos/archives?ref=manage&archive=1700000000000-nope&confirm=manage"); r.Code != http.StatusNotFound {
		t.Errorf("delete unknown = %d, want 404", r.Code)
	}
	if r := do(h, http.MethodDelete, "/api/v1/repos/archives?ref=team/manage&archive="+created.Archive.ID+"&confirm=manage"); r.Code != http.StatusOK {
		t.Fatalf("delete = %d (%s)", r.Code, r.Body.String())
	}
	list := do(h, http.MethodGet, "/api/v1/repos/archives?ref=manage")
	if !strings.Contains(list.Body.String(), `"total":0`) {
		t.Errorf("after delete list = %s", list.Body.String())
	}
	// 归档文件已删除
	if _, err := os.Stat(filepath.Join(archiveDir, repo.Name, created.Archive.ID+".bundle")); !os.IsNotExist(err) {
		t.Errorf("bundle should be deleted, stat err = %v", err)
	}

	// 空仓库手动快照 → 400
	if err := pgs.ReposManager.CreateRepository("emptyrepo", "", "master"); err != nil {
		t.Fatal(err)
	}
	if r := do(h, http.MethodPost, "/api/v1/repos/archives?ref=emptyrepo"); r.Code != http.StatusBadRequest {
		t.Errorf("snapshot empty repo = %d, want 400 (%s)", r.Code, r.Body.String())
	}
}
