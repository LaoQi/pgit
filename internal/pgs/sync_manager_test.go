package pgs

import (
	"testing"
)

func TestSyncManager_RegisterUnregister(t *testing.T) {
	InitSyncManager()
	defer func() { SyncMgr = nil }()

	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	mirror := &MirrorConfig{
		RemoteURL:    "https://example.com/repo.git",
		SyncInterval: 1,
	}
	ReposManager.CreateMirrorRepository("test-mirror", "", mirror)

	repo, _ := ReposManager.GetRepository("test-mirror")
	SyncMgr.Register(repo)

	SyncMgr.mu.Lock()
	_, ok := SyncMgr.mirrors["test-mirror"]
	SyncMgr.mu.Unlock()
	if !ok {
		t.Fatal("mirror not registered")
	}

	SyncMgr.Unregister("test-mirror")
	SyncMgr.mu.Lock()
	_, ok = SyncMgr.mirrors["test-mirror"]
	SyncMgr.mu.Unlock()
	if ok {
		t.Fatal("mirror still registered after unregister")
	}
}

func TestSyncManager_SyncNow(t *testing.T) {
	InitSyncManager()
	defer func() { SyncMgr = nil }()

	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()

	ReposManager.CreateRepository("normal", "", "master")

	_, err := SyncMgr.SyncNow("normal")
	if err == nil {
		t.Fatal("SyncNow should fail for non-mirror repo")
	}
}
