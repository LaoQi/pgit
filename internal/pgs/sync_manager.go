package pgs

import (
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// SyncManager 管理镜像仓库的定时同步与手动同步。
// 一个仓库同一时刻最多一个同步在跑（inflight 判重），定时调度器（scheduler）
// 与同步执行解耦：scheduler 只负责触发，手动同步不再需要预占槽位。
type SyncManager struct {
	manager *RepositoriesManager

	mu        sync.Mutex
	mirrors   map[string]*mirrorScheduler
	inflight  map[string]bool
	intervals map[string]int // repo -> 当前生效的调度间隔（间隔变更时重建）
	wg        sync.WaitGroup
}

type mirrorScheduler struct {
	stop chan struct{}
}

// NewSyncManager 构造镜像同步管理器，仓库元数据统一经 manager 访问。
func NewSyncManager(manager *RepositoriesManager) *SyncManager {
	return &SyncManager{
		manager:   manager,
		mirrors:   make(map[string]*mirrorScheduler),
		inflight:  make(map[string]bool),
		intervals: make(map[string]int),
	}
}

// Register 为镜像仓库注册定时调度。SyncInterval<=0 时不做任何事；
// 已有活跃调度器且间隔未变时保持原样（幂等，不会因手动同步等原因静默失效）；
// 间隔变更时重建（避免沿用旧 ticker 间隔）。
func (sm *SyncManager) Register(repo *Repository) {
	if repo == nil || repo.Mirror == nil || repo.Mirror.SyncInterval <= 0 {
		return
	}
	interval := repo.Mirror.SyncInterval // 快照：goroutine 不再读共享元数据

	sm.mu.Lock()
	if _, ok := sm.mirrors[repo.Name]; ok {
		if sm.intervals[repo.Name] == interval {
			sm.mu.Unlock()
			return // 已按同一间隔调度
		}
		// 间隔变更：关闭旧调度器后重建
		if old := sm.mirrors[repo.Name]; old.stop != nil {
			close(old.stop)
		}
		delete(sm.mirrors, repo.Name)
	}
	s := &mirrorScheduler{stop: make(chan struct{})}
	sm.mirrors[repo.Name] = s
	sm.intervals[repo.Name] = interval
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
	delete(sm.intervals, name)
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
	repo, err := sm.manager.GetRepository(name)
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
	result, err := sm.manager.SyncRepository(name)
	entry := &SyncLogEntry{
		Timestamp: start,
		Duration:  time.Since(start).Milliseconds(),
		Success:   err == nil,
		Trigger:   trigger,
	}
	if err != nil {
		entry.Error = err.Error()
		slog.Warn("mirror sync failed", "repo", name, "trigger", trigger, "error", err)
	} else {
		slog.Info("mirror sync ok", "repo", name, "trigger", trigger, "durationMs", entry.Duration)
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
	SetMirrorSyncResult(name, err == nil, entry.ObjectsFetch, entry.Duration)
	if repo, err := sm.manager.GetRepository(name); err == nil {
		if logErr := AppendSyncLog(repo.Path(), *entry); logErr != nil {
			slog.Warn("append sync log failed", "repo", name, "error", logErr)
		}
	}
	return entry, err
}

// MirrorStatus 是一个镜像仓库的调度与同步状态视图。
type MirrorStatus struct {
	Repo          string    `json:"repo"`
	Scheduled     bool      `json:"scheduled"`     // 是否有活跃定时调度器
	IntervalSec   int       `json:"intervalSec"`   // 调度间隔（秒），0=仅手动
	Syncing       bool      `json:"syncing"`       // 当前是否有同步在跑
	LastSync      time.Time `json:"lastSync"`      // 最近一次同步时间（含失败）
	LastError     string    `json:"lastError"`     // 最近一次同步错误
	NextScheduled time.Time `json:"nextScheduled"` // 下次定时触发（近似）
}

// Status 返回单个仓库的镜像状态；非镜像仓库返回错误。
func (sm *SyncManager) Status(name string) (*MirrorStatus, error) {
	repo, err := sm.manager.GetRepository(name)
	if err != nil {
		return nil, err
	}
	if !repo.IsMirror() {
		return nil, fmt.Errorf("repository %s is not a mirror", name)
	}

	sm.mu.Lock()
	_, scheduled := sm.mirrors[name]
	syncing := sm.inflight[name]
	sm.mu.Unlock()

	st := &MirrorStatus{
		Repo:        name,
		Scheduled:   scheduled,
		IntervalSec: repo.Mirror.SyncInterval,
		Syncing:     syncing,
		LastSync:    repo.Mirror.LastSync,
		LastError:   repo.Mirror.LastError,
	}
	if scheduled && repo.Mirror.SyncInterval > 0 {
		st.NextScheduled = time.Now().Add(time.Duration(repo.Mirror.SyncInterval) * time.Second)
	}
	return st, nil
}

// Statuses 返回全部镜像仓库的状态（按仓库名排序）。
func (sm *SyncManager) Statuses() []*MirrorStatus {
	repos := sm.manager.List()
	out := make([]*MirrorStatus, 0, len(repos))
	for _, repo := range repos {
		if !repo.IsMirror() {
			continue
		}
		if st, err := sm.Status(repo.Name); err == nil {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
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
	sm.intervals = make(map[string]int)
	sm.mu.Unlock()
	sm.wg.Wait()
}
