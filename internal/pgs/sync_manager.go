package pgs

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

const (
	// syncQueueCapacity 同步任务队列容量。队列满时定时任务被丢弃（饥饿可接受），
	// 手工同步直接返回错误。
	syncQueueCapacity = 1024
	// manualSyncQueueWait 手工同步在队列中的最长等待时间。超时返回 ErrSyncInProgress，
	// 避免 HTTP 请求无限期挂住（任务仍在队列中，随后照常执行）。
	manualSyncQueueWait = 30 * time.Second
)

// SyncManager 管理镜像仓库的定时同步与手动同步。
//
// 所有同步任务（首次/定时/手工/导入）统一提交到有界任务队列执行：
//   - 队列 worker 数 = 配置 mirrorMaxConcurrentSyncs（默认 5），可热调整；
//   - per-repo inflight 在「入队时」占位，保证同一仓库在队列中不会重复排队；
//   - 队列满时定时任务丢弃并计数（不阻塞、不重试），手工同步即时报错；
//   - 单任务执行时长由 fetch 自身的停滞超时/重试兜住，队列不设执行超时。
type SyncManager struct {
	manager *RepositoriesManager
	queue   *TaskQueue

	mu        sync.Mutex
	mirrors   map[string]*mirrorScheduler
	inflight  map[string]bool // 已入队或执行中
	active    map[string]bool // 正在执行（用于区分「排队中」与「执行中」）
	intervals map[string]int  // repo -> 当前生效的调度间隔（间隔变更时重建）
	wg        sync.WaitGroup
}

type mirrorScheduler struct {
	stop chan struct{}
}

// syncOutcome 是一次同步任务的结果（手工同步据此等待返回值）。
type syncOutcome struct {
	entry *SyncLogEntry
	err   error
}

// NewSyncManager 构造镜像同步管理器。worker 数取自全局配置（默认 5）。
func NewSyncManager(manager *RepositoriesManager) *SyncManager {
	return &SyncManager{
		manager:   manager,
		queue:     NewTaskQueue(syncQueueCapacity, Settings.LimitConcurrentSyncs()),
		mirrors:   make(map[string]*mirrorScheduler),
		inflight:  make(map[string]bool),
		active:    make(map[string]bool),
		intervals: make(map[string]int),
	}
}

// SetConcurrency 调整同步任务并发度（配置热加载后由调用方触发）。
func (sm *SyncManager) SetConcurrency(n int) { sm.queue.SetWorkers(n) }

// QueueStats 返回同步任务队列状态（顺带刷新相关指标）。
func (sm *SyncManager) QueueStats() QueueStats {
	st := sm.queue.Stats()
	SetSyncQueueStats(st)
	return st
}

// Register 为镜像仓库注册定时调度。SyncInterval<=0 时不做任何事；
// 已有活跃调度器且间隔未变时保持原样（幂等，不会因手动同步等原因静默失效）；
// 间隔变更时重建（避免沿用旧 ticker 间隔）。
//
// 注册即触发一次首次同步（入队，不阻塞调用方）。
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
		// 首次同步直接入队：并发由任务队列限制，无需错峰（旧实现的随机延迟已移除）。
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

// tryAcquire 尝试占用仓库的同步名额（入队即占位），返回是否成功。
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
	delete(sm.active, name)
	sm.mu.Unlock()
}

func (sm *SyncManager) markActive(name string, v bool) {
	sm.mu.Lock()
	if v {
		sm.active[name] = true
	} else {
		delete(sm.active, name)
	}
	sm.mu.Unlock()
}

// sendOutcome 非阻塞投递结果（等待方可能已超时离开）。
func sendOutcome(out chan syncOutcome, o syncOutcome) {
	if out == nil {
		return
	}
	select {
	case out <- o:
	default:
	}
}

// enqueue 占坑并把一次同步任务提交到队列。
// 重复提交（已在队列或执行中）返回 ErrSyncInProgress；队列满/已停止返回 ErrQueueFull。
func (sm *SyncManager) enqueue(name, trigger string, out chan syncOutcome) error {
	if !sm.tryAcquire(name) {
		return fmt.Errorf("%w: %s", ErrSyncInProgress, name)
	}
	enqueuedKeep := time.Now()
	task := &QueueTask{
		ID: "mirror-sync:" + name,
		Run: func() {
			defer sm.release(name)
			sm.markActive(name, true)
			defer sm.markActive(name, false)
			entry, err := sm.runSync(name, trigger, time.Since(enqueuedKeep))
			sendOutcome(out, syncOutcome{entry: entry, err: err})
		},
		Dropped: func() {
			// 队列满 / 队列已停止：让等待方立即拿到错误，而不是干等超时。
			sm.release(name)
			ObserveSyncTaskDropped(trigger)
			slog.Warn("mirror sync task dropped", "repo", name, "trigger", trigger)
			sendOutcome(out, syncOutcome{err: fmt.Errorf("%w: %s", ErrQueueFull, name)})
		},
	}
	if err := sm.queue.Submit(task); err != nil {
		sm.release(name)
		return fmt.Errorf("%w: %s (%v)", ErrQueueFull, name, err)
	}
	return nil
}

// doSync 定时/首次触发：入队失败即丢弃（不阻塞调度器、不重试）。
func (sm *SyncManager) doSync(name string, trigger string) {
	if err := sm.enqueue(name, trigger, nil); err != nil {
		slog.Warn("mirror sync not queued", "repo", name, "trigger", trigger, "error", err)
	}
}

// SyncNow 手动同步镜像仓库：入队并等待本次任务结果，返回本次同步日志条目。
func (sm *SyncManager) SyncNow(name string) (*SyncLogEntry, error) {
	repo, err := sm.manager.GetRepository(name)
	if err != nil {
		return nil, err
	}
	if !repo.IsMirror() {
		return nil, fmt.Errorf("%w: %s", ErrNotMirror, name)
	}
	out := make(chan syncOutcome, 1)
	if err := sm.enqueue(name, "manual", out); err != nil {
		return nil, err
	}
	timer := time.NewTimer(manualSyncQueueWait)
	defer timer.Stop()
	select {
	case o := <-out:
		return o.entry, o.err
	case <-timer.C:
		return nil, fmt.Errorf("%w: %s (排队超过 %s，任务仍在队列中)", ErrSyncInProgress, name, manualSyncQueueWait)
	}
}

// SyncAsync 提交一次异步同步（不等待结果）。用于「仅手动」镜像在创建后立即拉取首次内容：
// Register 对 SyncInterval<=0 的仓库不做调度，若不额外触发，导入出来的会是空壳仓库。
func (sm *SyncManager) SyncAsync(name string) error {
	repo, err := sm.manager.GetRepository(name)
	if err != nil {
		return err
	}
	if !repo.IsMirror() {
		return fmt.Errorf("%w: %s", ErrNotMirror, name)
	}
	return sm.enqueue(name, "import", nil)
}

// runSync 执行一次同步并写同步日志（由队列 worker 调用）。
func (sm *SyncManager) runSync(name string, trigger string, queueWait time.Duration) (*SyncLogEntry, error) {
	start := time.Now()
	result, err := sm.manager.SyncRepository(name)
	entry := &SyncLogEntry{
		Timestamp:   start,
		Duration:    time.Since(start).Milliseconds(),
		QueueWaitMs: queueWait.Milliseconds(),
		Success:     err == nil,
		Trigger:     trigger,
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
	ObserveSyncTask(trigger, map[bool]string{true: "success", false: "failure"}[err == nil])
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
	Queued        bool      `json:"queued"`        // 是否已在队列中排队（尚未开始执行）
	Syncing       bool      `json:"syncing"`       // 当前是否正在执行同步
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
		return nil, fmt.Errorf("%w: %s", ErrNotMirror, name)
	}

	sm.mu.Lock()
	_, scheduled := sm.mirrors[name]
	running := sm.active[name]
	inflight := sm.inflight[name]
	sm.mu.Unlock()

	st := &MirrorStatus{
		Repo:        name,
		Scheduled:   scheduled,
		IntervalSec: repo.Mirror.SyncInterval,
		Queued:      inflight && !running,
		Syncing:     running,
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

// Stop 停止全部调度器，然后停止任务队列（丢弃未开始的任务、等待运行中的任务收尾）。
// 可重复调用。
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
	sm.queue.Stop()
}
