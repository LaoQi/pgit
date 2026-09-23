package pgs

import (
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"
)

// SyncManager 管理镜像仓库的定时同步与手动同步。
// 一个仓库同一时刻最多一个同步在跑（inflight 判重），定时调度器（scheduler）
// 与同步执行解耦：scheduler 只负责触发，手动同步不再需要预占槽位。
type SyncManager struct {
	mu       sync.Mutex
	mirrors  map[string]*mirrorScheduler
	inflight map[string]bool
	wg       sync.WaitGroup
}

type mirrorScheduler struct {
	stop chan struct{}
}

var SyncMgr *SyncManager

func InitSyncManager() {
	SyncMgr = &SyncManager{
		mirrors:  make(map[string]*mirrorScheduler),
		inflight: make(map[string]bool),
	}
}

// Register 为镜像仓库注册定时调度。SyncInterval<=0 时不做任何事；
// 已有活跃调度器时保持原样（幂等），不会因手动同步等原因静默失效。
func (sm *SyncManager) Register(repo *Repository) {
	if repo == nil || repo.Mirror == nil || repo.Mirror.SyncInterval <= 0 {
		return
	}
	interval := repo.Mirror.SyncInterval // 快照：goroutine 不再读共享元数据

	sm.mu.Lock()
	if s, ok := sm.mirrors[repo.Name]; ok && s.stop != nil {
		sm.mu.Unlock()
		return
	}
	s := &mirrorScheduler{stop: make(chan struct{})}
	sm.mirrors[repo.Name] = s
	sm.wg.Add(1)
	sm.mu.Unlock()

	go func(name string) {
		defer sm.wg.Done()
		delay := time.Duration(1+rand.Intn(10)) * time.Second
		select {
		case <-time.After(delay):
		case <-s.stop:
			return
		}
		sm.doSync(name, "initial")
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sm.doSync(name, "scheduled")
			case <-s.stop:
				return
			}
		}
	}(repo.Name)
}

// Unregister 停止并移除指定仓库的调度器（不存在时无操作）。
func (sm *SyncManager) Unregister(name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.mirrors[name]
	if !ok {
		return
	}
	if s.stop != nil {
		close(s.stop)
	}
	delete(sm.mirrors, name)
}

// tryAcquire 尝试占用仓库的同步名额，返回是否成功。
func (sm *SyncManager) tryAcquire(name string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.inflight[name] {
		return false
	}
	sm.inflight[name] = true
	return true
}

func (sm *SyncManager) release(name string) {
	sm.mu.Lock()
	delete(sm.inflight, name)
	sm.mu.Unlock()
}

func (sm *SyncManager) doSync(name string, trigger string) {
	if !sm.tryAcquire(name) {
		return
	}
	defer sm.release(name)
	sm.runSync(name, trigger)
}

// SyncNow 手动同步镜像仓库，返回本次同步日志条目。
func (sm *SyncManager) SyncNow(name string) (*SyncLogEntry, error) {
	repo, err := ReposManager.GetRepository(name)
	if err != nil {
		return nil, err
	}
	if !repo.IsMirror() {
		return nil, fmt.Errorf("repository %s is not a mirror", name)
	}
	if !sm.tryAcquire(name) {
		return nil, fmt.Errorf("sync already in progress for %s", name)
	}
	defer sm.release(name)
	return sm.runSync(name, "manual")
}

// runSync 执行一次同步并写同步日志（调用方须已持有 inflight 名额）。
func (sm *SyncManager) runSync(name string, trigger string) (*SyncLogEntry, error) {
	start := time.Now()
	result, err := ReposManager.SyncRepository(name)
	entry := &SyncLogEntry{
		Timestamp: start,
		Duration:  time.Since(start).Milliseconds(),
		Success:   err == nil,
		Trigger:   trigger,
	}
	if err != nil {
		entry.Error = err.Error()
		log.Printf("sync %s [%s] failed: %v", name, trigger, err)
	} else {
		log.Printf("sync %s [%s] ok: duration=%dms", name, trigger, entry.Duration)
	}
	if result != nil {
		entry.ObjectsFetch = result.ObjectsWritten
		entry.RefsUpdated = result.RefsUpdated
		entry.RefsDeleted = result.RefsDeleted
		entry.UpToDate = result.UpToDate
		entry.Wants = result.Wants
		entry.Haves = result.Haves
		entry.PackSize = result.PackSize
	}
	if repo, err := ReposManager.GetRepository(name); err == nil {
		if logErr := AppendSyncLog(repo.Path(), *entry); logErr != nil {
			log.Printf("append sync log for %s failed: %v", name, logErr)
		}
	}
	return entry, err
}

// Stop 停止全部调度器并等待在跑的同步 goroutine 退出。可重复调用。
func (sm *SyncManager) Stop() {
	sm.mu.Lock()
	for _, s := range sm.mirrors {
		if s.stop != nil {
			close(s.stop)
		}
	}
	sm.mirrors = make(map[string]*mirrorScheduler)
	sm.mu.Unlock()
	sm.wg.Wait()
}
