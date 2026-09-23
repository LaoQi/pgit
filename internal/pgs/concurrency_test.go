package pgs

import (
	"fmt"
	"sync"
	"testing"
)

// newAuditManager 初始化使用临时 GitRoot 的 ReposManager（测试结束后还原全局）。
func newAuditManager(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { ReposManager = nil })
}

// 并发读（git 传输/列表）+ 并发写（管理 API）不得触发 data race 或
// 「concurrent map read and map write」进程级崩溃。
func TestConcurrentManagerAccess(t *testing.T) {
	newAuditManager(t)
	if err := ReposManager.CreateRepository("r1", "desc", "master"); err != nil {
		t.Fatal(err)
	}

	const readers, writers, iters = 8, 4, 100
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters*3; j++ {
				_ = ReposManager.List()
				_, _ = ReposManager.GetRepository("r1")
				_, _ = ReposManager.GetByAlias("r1")
				_ = ReposManager.RepositoryExist("r1")
			}
		}()
	}
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < iters/2; j++ {
				_ = ReposManager.AddAlias("r1", fmt.Sprintf("a%d-%d", i, j))
			}
		}(i)
	}
	wg.Add(1)
	go func() { // 建仓/删仓与读并发
		defer wg.Done()
		for j := 0; j < iters/2; j++ {
			name := fmt.Sprintf("tmp%d", j)
			if err := ReposManager.CreateRepository(name, "", ""); err == nil {
				_ = ReposManager.DeleteRepository(name)
			}
		}
	}()
	wg.Wait()

	repo, err := ReposManager.GetRepository("r1")
	if err != nil {
		t.Fatal(err)
	}
	want := 1 + writers*(iters/2)
	if len(repo.Aliases) != want {
		t.Errorf("aliases = %d, want %d (concurrent writes lost)", len(repo.Aliases), want)
	}
}

// 对外返回的元数据必须是快照：调用方修改不影响内部状态。
func TestRepositorySnapshotIsolation(t *testing.T) {
	newAuditManager(t)
	if err := ReposManager.CreateRepository("r1", "desc", "master"); err != nil {
		t.Fatal(err)
	}
	snap, err := ReposManager.GetRepository("r1")
	if err != nil {
		t.Fatal(err)
	}
	snap.Aliases = append(snap.Aliases, "hacked")
	snap.Description = "hacked"

	again, err := ReposManager.GetRepository("r1")
	if err != nil {
		t.Fatal(err)
	}
	if again.Description != "desc" || len(again.Aliases) != 1 {
		t.Errorf("internal state mutated through snapshot: desc=%q aliases=%v", again.Description, again.Aliases)
	}
	if _, err := ReposManager.GetByAlias("hacked"); err == nil {
		t.Error("alias added through snapshot leaked into index")
	}
}

// 手动同步之后 Register 仍须真正启动定时调度（回归：曾因 scheduler 占位而静默失效）。
func TestSyncRegisterAfterManualSync(t *testing.T) {
	newAuditManager(t)
	syncMgr := NewSyncManager(ReposManager)
	t.Cleanup(syncMgr.Stop)

	mirror := &MirrorConfig{RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 3600, AuthType: "none"}
	if err := ReposManager.CreateMirrorRepository("m1", "", mirror); err != nil {
		t.Fatal(err)
	}

	if _, err := syncMgr.SyncNow("m1"); err == nil {
		t.Log("SyncNow returned no error (unreachable remote is expected to fail)")
	}
	repo, _ := ReposManager.GetRepository("m1")
	syncMgr.Register(repo)

	syncMgr.mu.Lock()
	s := syncMgr.mirrors["m1"]
	running := s != nil && s.stop != nil
	syncMgr.mu.Unlock()
	if !running {
		t.Fatal("Register is a no-op after manual sync: scheduled sync would never start")
	}

	syncMgr.Stop()
	syncMgr.Stop() // 可重复调用
}

// Register 幂等；条件不满足（interval<=0 / 非镜像）时不注册。
func TestSyncRegisterIdempotent(t *testing.T) {
	newAuditManager(t)
	syncMgr := NewSyncManager(ReposManager)
	t.Cleanup(syncMgr.Stop)

	mirror := &MirrorConfig{RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 3600, AuthType: "none"}
	if err := ReposManager.CreateMirrorRepository("m1", "", mirror); err != nil {
		t.Fatal(err)
	}
	repo, _ := ReposManager.GetRepository("m1")
	syncMgr.Register(repo)
	syncMgr.Register(repo) // 第二次不得重建/泄漏 goroutine

	syncMgr.mu.Lock()
	n := len(syncMgr.mirrors)
	syncMgr.mu.Unlock()
	if n != 1 {
		t.Errorf("schedulers = %d, want 1", n)
	}

	manual := &MirrorConfig{RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 0, AuthType: "none"}
	if err := ReposManager.CreateMirrorRepository("m2", "", manual); err != nil {
		t.Fatal(err)
	}
	r2, _ := ReposManager.GetRepository("m2")
	syncMgr.Register(r2) // interval=0：仅手动，不注册调度

	syncMgr.mu.Lock()
	_, registered := syncMgr.mirrors["m2"]
	syncMgr.mu.Unlock()
	if registered {
		t.Error("interval=0 repo should not get a scheduler")
	}
	syncMgr.Stop()
}

// 同步（含失败路径）与改设置并发：状态回写与元数据落盘必须串行，不丢字段。
func TestConcurrentSyncAndSettingsUpdate(t *testing.T) {
	newAuditManager(t)
	mirror := &MirrorConfig{RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 3600, AuthType: "basic", Username: "u", Password: "p"}
	if err := ReposManager.CreateMirrorRepository("m2", "", mirror); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_, _ = ReposManager.SyncRepository("m2")
			}
		}()
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = ReposManager.UpdateRepositorySettings("m2", fmt.Sprintf("d%d-%d", i, j), &MirrorConfig{
					RemoteURL:    "http://127.0.0.1:1/none.git",
					SyncInterval: 3600,
					AuthType:     "basic",
					Username:     "u",
				})
			}
		}(i)
	}
	wg.Wait()

	repo, err := ReposManager.GetRepository("m2")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Mirror == nil || repo.Mirror.RemoteURL != "http://127.0.0.1:1/none.git" {
		t.Fatalf("mirror config lost: %+v", repo.Mirror)
	}
	if repo.Mirror.Password != "p" {
		t.Errorf("password = %q, want preserved %q", repo.Mirror.Password, "p")
	}
	if !repo.IsMirror() || repo.Mirror.LastSync.IsZero() {
		t.Error("sync status not persisted")
	}
}
