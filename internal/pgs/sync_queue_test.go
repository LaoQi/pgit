package pgs

import (
	"errors"
	"testing"
	"time"
)

func newQueueTestManager(t *testing.T) (*RepositoriesManager, *SyncManager) {
	t.Helper()
	dir := t.TempDir()
	GitRoot = dir
	InitReposManager(&RepositoriesManagerConfig{GitRoot: dir})
	t.Cleanup(func() { ReposManager = nil })

	mirror := &MirrorConfig{RemoteURL: "http://127.0.0.1:1/none.git", SyncInterval: 0, AuthType: "none"}
	if err := ReposManager.CreateMirrorRepository("m1", "", mirror); err != nil {
		t.Fatal(err)
	}
	sm := NewSyncManager(ReposManager)
	t.Cleanup(sm.Stop)
	return ReposManager, sm
}

// 排队即占坑：同一仓库不会重复入队，状态区分「排队中」与「执行中」。
func TestSyncManagerQueuedDedup(t *testing.T) {
	_, sm := newQueueTestManager(t)
	sm.SetConcurrency(1)
	if st := sm.QueueStats(); st.Workers != 1 {
		t.Fatalf("workers = %d, want 1 after SetConcurrency", st.Workers)
	}

	// 占住唯一的 worker，让同步任务只能排队
	blocker := make(chan struct{})
	blockerStarted := make(chan struct{})
	if err := sm.queue.Submit(&QueueTask{ID: "blocker", Run: func() {
		close(blockerStarted)
		<-blocker
	}}); err != nil {
		t.Fatal(err)
	}
	<-blockerStarted

	sm.doSync("m1", "scheduled")

	waitCond(t, func() bool {
		st, err := sm.Status("m1")
		return err == nil && st.Queued
	}, 2*time.Second)

	st, _ := sm.Status("m1")
	if st.Syncing {
		t.Error("task should be queued, not running")
	}
	// 排队中再次提交（含手工同步）应立刻返回已在进行中
	if _, err := sm.SyncNow("m1"); !errors.Is(err, ErrSyncInProgress) {
		t.Errorf("SyncNow while queued = %v, want ErrSyncInProgress", err)
	}
	if got := sm.QueueStats().Queued; got != 1 {
		t.Errorf("queued = %d, want 1 (no duplicate enqueue)", got)
	}

	close(blocker) // 放行 blocker，同步任务开始执行（远端不可达，最终失败）

	waitCond(t, func() bool {
		repo, err := sm.manager.GetRepository("m1")
		return err == nil && !repo.Mirror.LastSync.IsZero()
	}, 15*time.Second)
	repo, _ := sm.manager.GetRepository("m1")
	if repo.Mirror.LastError == "" {
		t.Error("unreachable remote should record lastError")
	}
	if st, _ := sm.Status("m1"); st.Queued || st.Syncing {
		t.Errorf("status after run = %+v, want idle", st)
	}
}

// 队列不可用时（已停止）手工同步立刻失败，而不是干等 30s。
func TestSyncManagerQueueClosedFailsFast(t *testing.T) {
	_, sm := newQueueTestManager(t)
	sm.queue.Stop()

	start := time.Now()
	_, err := sm.SyncNow("m1")
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("SyncNow took %s, want immediate failure", elapsed)
	}
	// 占位应已被释放
	sm.mu.Lock()
	inflight := sm.inflight["m1"]
	sm.mu.Unlock()
	if inflight {
		t.Error("inflight slot should be released after enqueue failure")
	}
}

// 手工同步返回的日志条目带 trigger 与排队耗时。
func TestSyncManagerManualEntryFields(t *testing.T) {
	_, sm := newQueueTestManager(t)
	entry, err := sm.SyncNow("m1")
	if err == nil {
		t.Fatal("sync to unreachable remote should fail")
	}
	if entry == nil {
		t.Fatal("entry should not be nil even on failure")
	}
	if entry.Trigger != "manual" {
		t.Errorf("trigger = %q, want manual", entry.Trigger)
	}
	if entry.QueueWaitMs < 0 {
		t.Errorf("queueWaitMs = %d, want >= 0", entry.QueueWaitMs)
	}
	if entry.Success {
		t.Error("entry should record failure")
	}
	logs, err := ReadSyncLog(mustRepoPath(t, sm.manager, "m1"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Trigger != "manual" {
		t.Errorf("sync log = %+v, want one manual entry", logs)
	}
}

// SyncAsync：仅手动镜像也能异步拉取一次（导入后立即镜像）。
func TestSyncManagerSyncAsync(t *testing.T) {
	_, sm := newQueueTestManager(t)
	if err := sm.SyncAsync("m1"); err != nil {
		t.Fatal(err)
	}
	waitCond(t, func() bool {
		repo, err := sm.manager.GetRepository("m1")
		return err == nil && !repo.Mirror.LastSync.IsZero()
	}, 15*time.Second)
	logs, err := ReadSyncLog(mustRepoPath(t, sm.manager, "m1"), 5)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %+v (%v), want 1 entry", logs, err)
	}
	if logs[0].Trigger != "import" {
		t.Errorf("trigger = %q, want import", logs[0].Trigger)
	}
	if err := sm.SyncAsync("missing"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("SyncAsync(missing) = %v, want ErrRepoNotFound", err)
	}
}

func mustRepoPath(t *testing.T, m *RepositoriesManager, name string) string {
	t.Helper()
	repo, err := m.GetRepository(name)
	if err != nil {
		t.Fatal(err)
	}
	return repo.Path()
}
