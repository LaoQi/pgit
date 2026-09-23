package pgs

import (
	"testing"
)

func TestSyncManager_RegisterUnregister(t *testing.T) {

	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	syncMgr := NewSyncManager(ReposManager)
	defer func() { ReposManager = nil }()

	mirror := &MirrorConfig{
		RemoteURL:    "https://example.com/repo.git",
		SyncInterval: 1,
	}
	ReposManager.CreateMirrorRepository("test-mirror", "", mirror)

	repo, _ := ReposManager.GetRepository("test-mirror")
	syncMgr.Register(repo)

	syncMgr.mu.Lock()
	_, ok := syncMgr.mirrors["test-mirror"]
	syncMgr.mu.Unlock()
	if !ok {
		t.Fatal("mirror not registered")
	}

	syncMgr.Unregister("test-mirror")
	syncMgr.mu.Lock()
	_, ok = syncMgr.mirrors["test-mirror"]
	syncMgr.mu.Unlock()
	if ok {
		t.Fatal("mirror still registered after unregister")
	}
}

func TestSyncManager_SyncNow(t *testing.T) {

	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	syncMgr := NewSyncManager(ReposManager)
	defer func() { ReposManager = nil }()

	ReposManager.CreateRepository("normal", "", "master")

	_, err := syncMgr.SyncNow("normal")
	if err == nil {
		t.Fatal("SyncNow should fail for non-mirror repo")
	}
}
