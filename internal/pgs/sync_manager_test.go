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

// Status/Statuses：镜像状态视图（调度中、间隔、同步中、最近同步/错误）。
func TestSyncManagerStatus(t *testing.T) {
	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()
	syncMgr := NewSyncManager(ReposManager)
	t.Cleanup(syncMgr.Stop)

	mirror := &MirrorConfig{RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 600, AuthType: "none"}
	if err := ReposManager.CreateMirrorRepository("m1", "", mirror); err != nil {
		t.Fatal(err)
	}
	repo, _ := ReposManager.GetRepository("m1")
	syncMgr.Register(repo)

	st, err := syncMgr.Status("m1")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Scheduled || st.IntervalSec != 600 {
		t.Errorf("status = %+v, want scheduled with 600s", st)
	}
	if st.NextScheduled.IsZero() {
		t.Error("nextScheduled should be set for scheduled mirrors")
	}

	// 首次同步（远端不可达）会记录 lastError
	_, _ = syncMgr.SyncNow("m1")
	st, _ = syncMgr.Status("m1")
	if st.LastError == "" {
		t.Error("lastError should be recorded after failed sync")
	}
	if st.LastSync.IsZero() {
		t.Error("lastSync should be recorded even on failure")
	}

	// Statuses 只列镜像仓库
	_ = ReposManager.CreateRepository("plain", "", "")
	list := syncMgr.Statuses()
	if len(list) != 1 || list[0].Repo != "m1" {
		t.Errorf("Statuses = %+v, want only m1", list)
	}

	// 非镜像仓库报错
	if _, err := syncMgr.Status("plain"); err == nil {
		t.Error("non-mirror repo should error")
	}
	// interval=0 的镜像仓库不调度
	manual := &MirrorConfig{RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 0, AuthType: "none"}
	if err := ReposManager.CreateMirrorRepository("m2", "", manual); err != nil {
		t.Fatal(err)
	}
	r2, _ := ReposManager.GetRepository("m2")
	syncMgr.Register(r2)
	st2, _ := syncMgr.Status("m2")
	if st2.Scheduled {
		t.Error("interval=0 mirror should not be scheduled")
	}
}

// 间隔变更时重建调度器（ticker 用新间隔），已同步状态不丢。
func TestSyncManagerIntervalChangeRebuilds(t *testing.T) {
	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	defer func() { ReposManager = nil }()
	syncMgr := NewSyncManager(ReposManager)
	t.Cleanup(syncMgr.Stop)

	mirror := &MirrorConfig{RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 600, AuthType: "none"}
	if err := ReposManager.CreateMirrorRepository("m1", "", mirror); err != nil {
		t.Fatal(err)
	}
	repo, _ := ReposManager.GetRepository("m1")
	syncMgr.Register(repo)

	syncMgr.mu.Lock()
	firstStop := syncMgr.mirrors["m1"].stop
	syncMgr.mu.Unlock()

	// 同间隔重复注册：不重建
	syncMgr.Register(repo)
	syncMgr.mu.Lock()
	sameStop := syncMgr.mirrors["m1"].stop
	syncMgr.mu.Unlock()
	if sameStop != firstStop {
		t.Error("same interval should not rebuild scheduler")
	}

	// 改间隔后注册：重建
	if _, err := ReposManager.UpdateRepositorySettings("m1", "", &MirrorConfig{
		RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 60, AuthType: "none",
	}); err != nil {
		t.Fatal(err)
	}
	updated, _ := ReposManager.GetRepository("m1")
	syncMgr.Register(updated)

	syncMgr.mu.Lock()
	newStop := syncMgr.mirrors["m1"].stop
	iv := syncMgr.intervals["m1"]
	syncMgr.mu.Unlock()
	if newStop == firstStop {
		t.Error("interval change should rebuild scheduler")
	}
	if iv != 60 {
		t.Errorf("recorded interval = %d, want 60", iv)
	}
	st, _ := syncMgr.Status("m1")
	if st.IntervalSec != 60 {
		t.Errorf("status interval = %d, want 60", st.IntervalSec)
	}
}
