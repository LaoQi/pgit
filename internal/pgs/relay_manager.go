package pgs

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"pgit/internal/pgs/git"
)

const (
	// relayQueueCapacity 转发任务队列容量。队列满时本轮入队失败：待转发 ref 已落盘在
	// PendingRefs，由后台重试或下一次 push 继续处理，不会丢失。
	relayQueueCapacity = 1024
	// relayManualWait 手动转发在队列中的最长等待时间。
	relayManualWait = 30 * time.Second
	// relayMaxRounds 一次任务内最多连续转发的轮次：push 期间涌入的新 ref 在后续轮次合并处理，
	// 避免单个任务长时间占用 worker（剩余部分交给下一轮任务/后台重试）。
	relayMaxRounds = 3
	// relayDefaultRetryInterval / relayMaxRetryInterval：存在未转发成功的 ref 时的
	// 后台重试间隔与退避上限（间隔配置为 relayPendingRetryIntervalSec）。
	relayDefaultRetryInterval = 60 * time.Second
	relayMaxRetryInterval     = 600 * time.Second
)

// RelayManager 管理中转仓库的「下游推送准入 + 转发上游」。
//
// 与 SyncManager（拉取方向）对称，共享同一套基础设施：
//   - 所有转发任务统一提交到有界任务队列，per-repo 去重（入队即占位）；
//   - 待转发 ref 落盘在 MirrorConfig.PendingRefs，是「尚未转发成功」的权威集合，
//     重启后据此补推；
//   - 推送失败（网络/上游瞬时故障）保留 pending 并后台退避重试；
//     上游明确拒绝（ng）或策略不允许的 ref 只记录错误、不再重试，避免无限循环。
type RelayManager struct {
	manager *RepositoriesManager
	queue   *TaskQueue

	mu      sync.Mutex
	states  map[string]*relayState
	bases   map[string]*relayBase
	stopped bool
	wg      sync.WaitGroup
}

type relayState struct {
	queued     bool
	running    bool
	nextRetry  time.Time
	retryTimer *time.Timer
	retryDelay time.Duration
}

// relayOutcome 是一次手动转发的结果。
type relayOutcome struct {
	entry *RelayLogEntry
	err   error
}

// NewRelayManager 构造中转管理器。worker 数取自全局配置（默认 5）。
func NewRelayManager(manager *RepositoriesManager) *RelayManager {
	return &RelayManager{
		manager: manager,
		queue:   NewTaskQueue(relayQueueCapacity, Settings.LimitConcurrentPushes()),
		states:  make(map[string]*relayState),
		bases:   make(map[string]*relayBase),
	}
}

// SetConcurrency 调整转发任务并发度（配置热加载后由调用方触发）。
func (rm *RelayManager) SetConcurrency(n int) { rm.queue.SetWorkers(n) }

// QueueStats 返回转发任务队列状态（顺带刷新指标）。
func (rm *RelayManager) QueueStats() QueueStats {
	st := rm.queue.Stats()
	SetRelayQueueStats(st)
	return st
}

func (rm *RelayManager) stateFor(name string) *relayState {
	st, ok := rm.states[name]
	if !ok {
		st = &relayState{}
		rm.states[name] = st
	}
	return st
}

// baseFor 取（或惰性创建）仓库的上游基线视图。并发安全：
// Admit/refreshBase/applyRound 等多条路径并发调用，必须持锁访问 rm.bases。
func (rm *RelayManager) baseFor(name string) *relayBase {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.baseForLocked(name)
}

// baseForLocked 是 baseFor 的无锁版本，仅供已持有 rm.mu 的调用方使用（如 Status）。
func (rm *RelayManager) baseForLocked(name string) *relayBase {
	b, ok := rm.bases[name]
	if !ok {
		b = newRelayBase()
		rm.bases[name] = b
	}
	return b
}

// Admit 是 receive-pack 的 ref 准入校验（在对象落盘后、ref 更新前由 git 层回调）。
// 返回与 updates 等长的结果切片：nil 元素表示放行，非 nil 表示拒绝并携带给客户端的原因。
//
// 放行条件（保证转发必然是一次可直通上游的快进推送）：
//   - ref 在白名单内（默认 refs/heads/ 与 refs/tags/）
//   - 上游基线可用（每次都实时校准）
//   - 上游无该 ref：允许新建（客户端 old 必须为零）
//   - 上游有该 ref：old 必须等于基线 oid，且 new 是基线的后代（删除还要求允许删除）
func (rm *RelayManager) Admit(repo *Repository, updates []git.RefUpdate) []*git.RefUpdateResult {
	if repo == nil || !repo.IsRelay() || len(updates) == 0 {
		return nil
	}
	m := repo.Mirror

	base, err := rm.refreshBase(repo)
	if err != nil {
		reason := fmt.Sprintf("relay: upstream base unavailable: %v", err)
		ObserveRelayAdmit(repo.Name, "error")
		slog.Warn("relay admit rejected: base unavailable", "repo", repo.Name, "error", err)
		out := make([]*git.RefUpdateResult, len(updates))
		for i, u := range updates {
			out[i] = &git.RefUpdateResult{Name: u.Name, Reason: reason}
		}
		return out
	}

	store := git.NewObjectStore(repo.Path())
	out := make([]*git.RefUpdateResult, len(updates))
	rejected := 0
	for i, u := range updates {
		if reason := admitOne(m, store, base, u); reason != "" {
			out[i] = &git.RefUpdateResult{Name: u.Name, Reason: reason}
			rejected++
			slog.Warn("relay admit rejected", "repo", repo.Name, "ref", u.Name,
				"old", u.OldOid.String(), "new", u.NewOid.String(), "reason", reason)
		}
	}
	if rejected > 0 {
		ObserveRelayAdmit(repo.Name, "rejected")
	} else {
		ObserveRelayAdmit(repo.Name, "accepted")
	}
	return out
}

// admitOne 对单个 ref 做准入判定，返回空字符串表示放行。
func admitOne(m *MirrorConfig, store git.ObjectStore, base *relayBase, u git.RefUpdate) string {
	if !m.AcceptsRef(u.Name) {
		return fmt.Sprintf("relay: ref not forwarded (allowed prefixes: %s)",
			strings.Join(m.ForwardRefPrefixes(), ", "))
	}
	if !u.NewOid.IsZero() && !u.NewOid.Valid() {
		return "relay: invalid new oid"
	}
	if !u.OldOid.IsZero() && !u.OldOid.Valid() {
		return "relay: invalid old oid"
	}

	baseOid, known := base.Oid(u.Name)
	if !known || baseOid.IsZero() {
		// 上游没有该 ref：允许新建（tag / 新分支）。客户端若带着非零 old（本地以为上游有），
		// 说明它的基线不是当前上游，要求先 fetch。
		if !u.OldOid.IsZero() {
			return fmt.Sprintf("relay: stale base (upstream has no %s); fetch and retry", u.Name)
		}
		return ""
	}

	if u.OldOid != baseOid {
		return fmt.Sprintf("relay: stale base (upstream %s is %s); fetch and retry", u.Name, shortOid(baseOid))
	}
	if u.NewOid.IsZero() {
		if !m.AllowsDelete() {
			return "relay: delete of upstream ref is disabled for this repository"
		}
		return ""
	}
	if !git.IsFastForward(store, baseOid, u.NewOid) {
		return fmt.Sprintf("relay: non-fast-forward push rejected (upstream base is %s)", shortOid(baseOid))
	}
	return ""
}

// OnRefsUpdated 由接收下游 push 的传输层在 ref 更新成功后调用（非阻塞）。
// 把成功更新且在白名单内的 ref 记入待转发集合并提交转发任务。
func (rm *RelayManager) OnRefsUpdated(repo *Repository, results []git.RefUpdateResult) {
	if repo == nil || !repo.IsRelay() || len(results) == 0 {
		return
	}
	m := repo.Mirror
	refs := make([]string, 0, len(results))
	for _, r := range results {
		if !r.Ok || r.Name == "" || !m.AcceptsRef(r.Name) {
			continue
		}
		refs = append(refs, r.Name)
	}
	if len(refs) == 0 {
		return
	}
	if total, err := rm.markPending(repo.Name, refs); err != nil {
		slog.Warn("relay mark pending failed", "repo", repo.Name, "error", err)
	} else {
		// gauge 语义：上报合并后的待转发总数（而非本轮新增数）
		ObserveRelayPending(repo.Name, total)
	}
	if err := rm.enqueue(repo.Name, "push", nil); err != nil {
		// 队列满/已停止：pending 已落盘，由后台重试处理
		slog.Warn("relay task not queued", "repo", repo.Name, "error", err)
	}
}

// RelayNow 手动触发一次转发：把「本地领先于上游基线」的 ref 纳入待转发集合并等待结果。
func (rm *RelayManager) RelayNow(name string) (*RelayLogEntry, error) {
	repo, err := rm.manager.GetRepository(name)
	if err != nil {
		return nil, err
	}
	if !repo.IsRelay() {
		return nil, fmt.Errorf("%w: %s", ErrNotRelay, name)
	}
	base, err := rm.refreshBase(repo)
	if err != nil {
		return nil, err
	}
	baseRefs, _ := base.Snapshot()
	ahead := rm.localAheadRefs(repo, baseRefs)
	if len(ahead) > 0 {
		if _, err := rm.markPending(name, ahead); err != nil {
			return nil, err
		}
	}
	out := make(chan relayOutcome, 1)
	if err := rm.enqueue(name, "manual", out); err != nil {
		return nil, err
	}
	timer := time.NewTimer(relayManualWait)
	defer timer.Stop()
	select {
	case o := <-out:
		return o.entry, o.err
	case <-timer.C:
		return nil, fmt.Errorf("%w: %s (排队超过 %s，任务仍在队列中)", ErrSyncInProgress, name, relayManualWait)
	}
}

// Bootstrap 在启动时为中转仓库做初始化：补推上次未完成的转发，并异步校准基线。
func (rm *RelayManager) Bootstrap(repo *Repository) {
	if repo == nil || !repo.IsRelay() {
		return
	}
	if len(repo.Mirror.PendingRefs) > 0 {
		slog.Info("relay pending refs from previous run, scheduling forward",
			"repo", repo.Name, "refs", repo.Mirror.PendingRefs)
		if err := rm.enqueue(repo.Name, "startup", nil); err != nil {
			slog.Warn("relay startup forward not queued", "repo", repo.Name, "error", err)
		}
	}
	name := repo.Name
	rm.wg.Add(1)
	go func() {
		defer rm.wg.Done()
		r, err := rm.manager.GetRepository(name)
		if err != nil {
			return
		}
		if _, err := rm.refreshBase(r); err != nil {
			slog.Warn("relay base calibration failed", "repo", name, "error", err)
		}
	}()
}

// Unregister 清理仓库的运行时状态（软删除/配置切换时调用）。
func (rm *RelayManager) Unregister(name string) {
	rm.mu.Lock()
	st := rm.states[name]
	if st != nil && st.retryTimer != nil {
		st.retryTimer.Stop()
	}
	delete(rm.states, name)
	delete(rm.bases, name)
	rm.mu.Unlock()
}

// Stop 停止全部转发调度与任务队列（可重复调用）。
func (rm *RelayManager) Stop() {
	rm.mu.Lock()
	rm.stopped = true
	for _, st := range rm.states {
		if st.retryTimer != nil {
			st.retryTimer.Stop()
		}
	}
	rm.mu.Unlock()
	rm.wg.Wait()
	rm.queue.Stop()
}

// refreshBase 实时校准上游基线（ls-remote，不传输对象）。
func (rm *RelayManager) refreshBase(repo *Repository) (*relayBase, error) {
	m := repo.Mirror
	refs, _, err := git.LsRemote(m.RemoteURL, relayAuth(m), Settings.MirrorFetchOptions(), "git-receive-pack")
	if err != nil {
		return rm.baseFor(repo.Name), err
	}
	b := rm.baseFor(repo.Name)
	b.Set(refs)
	return b, nil
}

// enqueue 提交一次转发任务。同一仓库已在队列中或执行中时直接返回：
// 待转发集合已落盘，运行中的任务结束前会检查到它们。
func (rm *RelayManager) enqueue(name, trigger string, out chan relayOutcome) error {
	rm.mu.Lock()
	if rm.stopped {
		rm.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrQueueClosed, name)
	}
	st := rm.stateFor(name)
	if st.queued || st.running {
		rm.mu.Unlock()
		if out != nil {
			return fmt.Errorf("%w: %s", ErrSyncInProgress, name)
		}
		return nil
	}
	st.queued = true
	rm.mu.Unlock()

	task := &QueueTask{
		ID: "relay-forward:" + name,
		Run: func() {
			rm.mu.Lock()
			st.queued = false
			st.running = true
			rm.mu.Unlock()

			entry, err := rm.runRounds(name, trigger)

			rm.mu.Lock()
			st.running = false
			rm.mu.Unlock()
			if out != nil {
				select {
				case out <- relayOutcome{entry: entry, err: err}:
				default:
				}
			}
		},
		Dropped: func() {
			rm.mu.Lock()
			st.queued = false
			rm.mu.Unlock()
			ObserveRelayTaskDropped(trigger)
			slog.Warn("relay forward task dropped", "repo", name, "trigger", trigger)
			if out != nil {
				select {
				case out <- relayOutcome{err: fmt.Errorf("%w: %s", ErrQueueFull, name)}:
				default:
				}
			}
		},
	}
	if err := rm.queue.Submit(task); err != nil {
		rm.mu.Lock()
		st.queued = false
		rm.mu.Unlock()
		return fmt.Errorf("%w: %s (%v)", ErrQueueFull, name, err)
	}
	return nil
}

// runRounds 处理待转发集合，最多连续 relayMaxRounds 轮（把期间的并发 push 合并进来）。
func (rm *RelayManager) runRounds(name, trigger string) (*RelayLogEntry, error) {
	var last *RelayLogEntry
	var lastErr error
	for round := 0; round < relayMaxRounds; round++ {
		repo, err := rm.manager.GetRepository(name)
		if err != nil {
			return last, err
		}
		if !repo.IsRelay() {
			return last, fmt.Errorf("%w: %s", ErrNotRelay, name)
		}
		pending := append([]string(nil), repo.Mirror.PendingRefs...)
		if len(pending) == 0 {
			break
		}
		entry, err := rm.runRound(repo, pending, trigger)
		last, lastErr = entry, err
		if err != nil {
			break // 整包失败（网络/协议）：pending 保留，交给后台重试
		}
		if entry != nil && !entry.Success {
			// 仍有未成功的 ref（上游拒绝等）：不再自动重试
			break
		}
	}
	rm.updateRetry(name)
	return last, lastErr
}

// runRound 执行一轮转发：以「本地当前 ref 值」为内容，把 pending 里的 ref 推给上游。
func (rm *RelayManager) runRound(repo *Repository, pending []string, trigger string) (*RelayLogEntry, error) {
	start := time.Now()
	m := repo.Mirror

	rs := git.NewRefStore(repo.Path())
	specs := make([]git.RefSpec, 0, len(pending))
	var refused []string // 因策略直接放弃（不再重试）的 ref
	for _, ref := range pending {
		oid, err := rs.Get(ref)
		if err == nil && oid.Valid() && !oid.IsZero() {
			specs = append(specs, git.RefSpec{Ref: ref, Oid: oid})
			continue
		}
		if !m.AllowsDelete() {
			refused = append(refused, ref)
			continue
		}
		specs = append(specs, git.RefSpec{Ref: ref, Oid: git.ZeroOid})
	}

	entry := &RelayLogEntry{
		Timestamp: start,
		Trigger:   trigger,
		Refs:      append([]string(nil), pending...),
	}

	if len(specs) == 0 {
		entry.Success = false
		entry.Error = fmt.Sprintf("relay: deleting upstream refs is disabled (%s)", strings.Join(refused, ", "))
		entry.Duration = time.Since(start).Milliseconds()
		rm.applyRound(repo.Name, nil, refused, entry.Error)
		rm.appendLog(repo, entry)
		return entry, nil
	}

	result, err := git.PushRemote(m.RemoteURL, repo.Path(), specs, relayAuth(m), Settings.MirrorFetchOptions())
	entry.Duration = time.Since(start).Milliseconds()
	if err != nil {
		entry.Success = false
		entry.Error = err.Error()
		rm.applyRound(repo.Name, nil, nil, entry.Error) // 全部保留 pending
		rm.appendLog(repo, entry)
		ObserveRelayPush(repo.Name, false, 0, 0, len(specs), 0, entry.Duration, 0)
		slog.Warn("relay forward failed", "repo", repo.Name, "trigger", trigger,
			"refs", len(specs), "error", err)
		return entry, err
	}

	var done []git.PushRefResult
	var rejected []string // 上游明确拒绝：不再重试，但保留错误提示
	for _, r := range result.Refs {
		switch {
		case r.Ok && r.NewOid.IsZero():
			entry.RefsDeleted++
			done = append(done, r)
		case r.Ok:
			entry.RefsPushed++
			done = append(done, r)
		default:
			entry.RefsRejected++
			rejected = append(rejected, fmt.Sprintf("%s: %s", r.Ref, r.Reason))
		}
	}
	entry.UpToDate = result.UpToDate
	entry.Objects = result.ObjectsSent
	entry.PackSize = result.PackSize
	entry.Success = result.AllOK()

	errMsg := ""
	switch {
	case entry.Error != "":
	case len(rejected) > 0:
		errMsg = "relay: upstream rejected " + strings.Join(rejected, "; ")
	case len(refused) > 0:
		errMsg = "relay: deleting upstream refs is disabled (" + strings.Join(refused, ", ") + ")"
	}
	entry.Error = errMsg

	// 成功者从 pending 移除并推进基线；上游拒绝/策略拒绝者放弃重试（否则会无限循环）。
	drop := make([]string, 0, len(rejected))
	for _, r := range result.Refs {
		if !r.Ok {
			drop = append(drop, r.Ref)
		}
	}
	drop = append(drop, refused...)
	rm.applyRound(repo.Name, done, drop, errMsg)
	rm.appendLog(repo, entry)
	ObserveRelayPush(repo.Name, entry.Success, entry.RefsPushed, entry.RefsDeleted, entry.RefsRejected,
		entry.Objects, entry.Duration, entry.PackSize)
	slog.Info("relay forward done", "repo", repo.Name, "trigger", trigger,
		"pushed", entry.RefsPushed, "deleted", entry.RefsDeleted, "rejected", entry.RefsRejected,
		"objects", entry.Objects, "bytes", entry.PackSize, "error", entry.Error)
	return entry, nil
}

// applyRound 在锁内回写运行态：成功者推进基线并移出 pending，
// drop 中的 ref 放弃重试（上游拒绝/策略不允许），errMsg 记入 LastPushError。
func (rm *RelayManager) applyRound(name string, done []git.PushRefResult, drop []string, errMsg string) {
	err := rm.manager.UpdateMirrorRuntime(name, func(m *MirrorConfig) {
		clear := make(map[string]bool, len(done)+len(drop))
		for _, r := range done {
			if !r.Ok {
				continue
			}
			clear[r.Ref] = true
			b := rm.baseFor(name)
			if r.NewOid.IsZero() {
				b.Remove(r.Ref)
			} else {
				b.Advance(r.Ref, r.NewOid)
			}
		}
		for _, ref := range drop {
			clear[ref] = true
		}
		if len(clear) > 0 {
			kept := make([]string, 0, len(m.PendingRefs))
			for _, ref := range m.PendingRefs {
				if !clear[ref] {
					kept = append(kept, ref)
				}
			}
			m.PendingRefs = kept
		}
		if len(done) > 0 || len(drop) > 0 {
			m.LastPush = time.Now()
		}
		m.LastPushError = errMsg
		ObserveRelayPending(name, len(m.PendingRefs))
	})
	if err != nil {
		slog.Warn("relay update runtime failed", "repo", name, "error", err)
	}
}

// updateRetry 依据剩余 pending 安排/取消后台重试。
func (rm *RelayManager) updateRetry(name string) {
	repo, err := rm.manager.GetRepository(name)
	if err != nil || repo == nil || !repo.IsRelay() {
		return
	}
	if len(repo.Mirror.PendingRefs) == 0 {
		rm.clearRetry(name)
		return
	}
	interval := Settings.RelayRetryInterval()
	if interval <= 0 {
		return // 后台重试被配置关闭：留待下一次 push 或手动触发
	}

	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.stopped {
		return
	}
	st := rm.stateFor(name)
	if st.retryDelay <= 0 {
		st.retryDelay = interval
	} else {
		st.retryDelay *= 2
		if st.retryDelay > relayMaxRetryInterval {
			st.retryDelay = relayMaxRetryInterval
		}
	}
	if st.retryTimer != nil {
		st.retryTimer.Stop()
	}
	delay := st.retryDelay
	st.nextRetry = time.Now().Add(delay)
	st.retryTimer = time.AfterFunc(delay, func() {
		rm.mu.Lock()
		if rm.stopped {
			rm.mu.Unlock()
			return
		}
		st.nextRetry = time.Time{}
		rm.mu.Unlock()
		if err := rm.enqueue(name, "retry", nil); err != nil {
			slog.Warn("relay retry not queued", "repo", name, "error", err)
		}
	})
	slog.Info("relay retry scheduled", "repo", name, "in", delay.Round(time.Second),
		"pending", repo.Mirror.PendingRefs)
}

func (rm *RelayManager) clearRetry(name string) {
	rm.mu.Lock()
	st := rm.states[name]
	if st != nil {
		if st.retryTimer != nil {
			st.retryTimer.Stop()
			st.retryTimer = nil
		}
		st.retryDelay = 0
		st.nextRetry = time.Time{}
	}
	rm.mu.Unlock()
}

// markPending 把 refs 并入待转发集合（落盘），返回更新后的待转发总数。
func (rm *RelayManager) markPending(name string, refs []string) (int, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	total := 0
	err := rm.manager.UpdateMirrorRuntime(name, func(m *MirrorConfig) {
		set := make(map[string]bool, len(m.PendingRefs)+len(refs))
		for _, ref := range m.PendingRefs {
			set[ref] = true
		}
		for _, ref := range refs {
			set[ref] = true
		}
		m.PendingRefs = sortedKeys(set)
		total = len(m.PendingRefs)
	})
	return total, err
}

// localAheadRefs 返回「本地领先于上游基线」的 ref（可安全转发）：
// 上游没有该 ref，或本地 oid 是基线 oid 的快进后代。本地落后的分支不在此列（需先拉取）。
func (rm *RelayManager) localAheadRefs(repo *Repository, base map[string]git.Oid) []string {
	rs := git.NewRefStore(repo.Path())
	store := git.NewObjectStore(repo.Path())
	refs, err := rs.List()
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range refs {
		if r.Name == "HEAD" || !repo.Mirror.AcceptsRef(r.Name) {
			continue
		}
		baseOid, ok := base[r.Name]
		if !ok || baseOid.IsZero() || baseOid == r.Oid {
			if !ok || baseOid.IsZero() {
				out = append(out, r.Name) // 上游没有 → 新建
			}
			continue
		}
		if git.IsFastForward(store, baseOid, r.Oid) {
			out = append(out, r.Name)
		}
	}
	return out
}

// appendLog 写一条转发日志（失败仅记警告，不影响转发结果）。
func (rm *RelayManager) appendLog(repo *Repository, entry *RelayLogEntry) {
	if entry == nil {
		return
	}
	if err := AppendRelayLog(repo.Path(), *entry); err != nil {
		slog.Warn("append relay log failed", "repo", repo.Name, "error", err)
	}
}

// RelayAlignResult 是对齐操作的结果。
type RelayAlignResult struct {
	BaseRefs int      `json:"baseRefs"`
	Updated  int      `json:"updated"`
	Deleted  int      `json:"deleted"`
	Aligned  []string `json:"aligned"`
}

// AlignToUpstream 把本地 refs 强制对齐上游基线，并丢弃待转发集合。
//
// 这是危险运维出口：用于「转发被上游永久拒绝/长期失败」后恢复可用状态。
// 对齐会丢弃本地领先的提交的 ref 指向（对象仍留在本地磁盘，无 GC），
// 因此调用方（HTTP 层）必须要求显式确认。
func (rm *RelayManager) AlignToUpstream(name string) (*RelayAlignResult, error) {
	repo, err := rm.manager.GetRepository(name)
	if err != nil {
		return nil, err
	}
	if !repo.IsRelay() {
		return nil, fmt.Errorf("%w: %s", ErrNotRelay, name)
	}
	base, err := rm.refreshBase(repo)
	if err != nil {
		return nil, err
	}
	baseRefs, _ := base.Snapshot()

	rs := git.NewRefStore(repo.Path())
	localList, err := rs.List()
	if err != nil {
		return nil, err
	}
	localOids := make(map[string]git.Oid, len(localList))
	for _, l := range localList {
		if l.Name == "HEAD" {
			continue
		}
		localOids[l.Name] = l.Oid
	}

	var updates []git.RefUpdate
	var changed []string
	for ref, oid := range baseRefs {
		if !repo.Mirror.AcceptsRef(ref) {
			continue
		}
		if cur, ok := localOids[ref]; ok && cur == oid {
			continue
		}
		updates = append(updates, git.RefUpdate{Name: ref, OldOid: localOids[ref], NewOid: oid})
		changed = append(changed, ref)
	}
	for ref, oid := range localOids {
		if !repo.Mirror.AcceptsRef(ref) {
			continue
		}
		if _, ok := baseRefs[ref]; ok {
			continue
		}
		updates = append(updates, git.RefUpdate{Name: ref, OldOid: oid, NewOid: git.ZeroOid})
		changed = append(changed, ref)
	}

	updated, deleted := 0, 0
	if len(updates) > 0 {
		results, err := rs.Update(updates)
		if err != nil {
			return nil, fmt.Errorf("align: update refs: %w", err)
		}
		for i, u := range updates {
			if i < len(results) && results[i].Ok {
				if u.NewOid.IsZero() {
					deleted++
				} else {
					updated++
				}
			}
		}
	}

	// 本地已与上游对齐：丢弃待转发集合与错误状态
	if err := rm.manager.UpdateMirrorRuntime(name, func(m *MirrorConfig) {
		m.PendingRefs = nil
		m.LastPushError = ""
	}); err != nil {
		return nil, err
	}
	rm.clearRetry(name)
	sort.Strings(changed)
	slog.Warn("relay aligned to upstream", "repo", name, "changed", changed)
	return &RelayAlignResult{
		BaseRefs: len(baseRefs),
		Updated:  updated,
		Deleted:  deleted,
		Aligned:  changed,
	}, nil
}

// RelayStatus 是中转仓库的状态视图（供 API/WebUI 展示）。
type RelayStatus struct {
	Repo          string     `json:"repo"`
	Upstream      string     `json:"upstream"`
	Queued        bool       `json:"queued"`
	Pushing       bool       `json:"pushing"`
	PendingRefs   []string   `json:"pendingRefs"`
	LastPush      time.Time  `json:"lastPush"`
	LastPushError string     `json:"lastPushError"`
	BaseRefs      int        `json:"baseRefs"`
	BaseAt        time.Time  `json:"baseAt"`
	Differ        []string   `json:"differ"` // 本地与上游基线不一致的 ref（push 前需先同步）
	NextRetry     *time.Time `json:"nextRetry,omitempty"`
}

// Status 返回单个中转仓库的状态；非中转仓库返回 ErrNotRelay。
func (rm *RelayManager) Status(name string) (*RelayStatus, error) {
	repo, err := rm.manager.GetRepository(name)
	if err != nil {
		return nil, err
	}
	if !repo.IsRelay() {
		return nil, fmt.Errorf("%w: %s", ErrNotRelay, name)
	}

	rm.mu.Lock()
	st := rm.states[name]
	queued, running := false, false
	var nextRetry *time.Time
	if st != nil {
		queued, running = st.queued || st.running, st.running
		if !st.nextRetry.IsZero() {
			t := st.nextRetry
			nextRetry = &t
		}
	}
	baseRefs, baseAt := rm.baseForLocked(name).Snapshot()
	rm.mu.Unlock()

	status := &RelayStatus{
		Repo:          name,
		Upstream:      repo.Mirror.RemoteURL,
		Queued:        queued,
		Pushing:       running,
		PendingRefs:   append([]string{}, repo.Mirror.PendingRefs...),
		LastPush:      repo.Mirror.LastPush,
		LastPushError: repo.Mirror.LastPushError,
		BaseRefs:      len(baseRefs),
		BaseAt:        baseAt,
		NextRetry:     nextRetry,
	}
	if baseAt.IsZero() {
		// 基线尚未校准：differ 无从计算（否则会把每个本地 ref 都报成差异）。
		// 顺手异步校准一次，让后续查询/推送有基线可用。
		rm.calibrateAsync(name)
	} else {
		status.Differ = differRefs(repo, baseRefs)
	}
	return status, nil
}

// calibrateAsync 在后台校准一次上游基线（不阻塞调用方）。
func (rm *RelayManager) calibrateAsync(name string) {
	rm.wg.Add(1)
	go func() {
		defer rm.wg.Done()
		repo, err := rm.manager.GetRepository(name)
		if err != nil || !repo.IsRelay() {
			return
		}
		if _, err := rm.refreshBase(repo); err != nil {
			slog.Debug("relay base calibration failed", "repo", name, "error", err)
		}
	}()
}

// Statuses 返回全部中转仓库的状态（按仓库名排序）。
func (rm *RelayManager) Statuses() []*RelayStatus {
	repos := rm.manager.List()
	out := make([]*RelayStatus, 0)
	for _, repo := range repos {
		if !repo.IsRelay() {
			continue
		}
		if st, err := rm.Status(repo.Name); err == nil {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

// differRefs 列出本地 refs 与上游基线不一致的 ref（提示用户先 fetch 再 push）。
func differRefs(repo *Repository, base map[string]git.Oid) []string {
	rs := git.NewRefStore(repo.Path())
	refs, err := rs.List()
	if err != nil {
		return nil
	}
	local := make(map[string]git.Oid, len(refs))
	for _, r := range refs {
		if r.Name == "HEAD" || !repo.Mirror.AcceptsRef(r.Name) {
			continue
		}
		local[r.Name] = r.Oid
	}
	var out []string
	for name, oid := range local {
		if b, ok := base[name]; !ok || b != oid {
			out = append(out, name)
		}
	}
	for name := range base {
		if _, ok := local[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// relayAuth 由远端配置构造认证信息（与镜像同步共用同一套语义）。
func relayAuth(m *MirrorConfig) *git.FetchAuth {
	if m == nil {
		return nil
	}
	if m.AuthType == "basic" || m.Proxy != "" {
		return &git.FetchAuth{
			Type:     m.AuthType,
			Username: m.Username,
			Password: m.Password,
			Proxy:    m.Proxy,
		}
	}
	return nil
}

// shortOid 取 oid 前 7 位（无则原样）。
func shortOid(oid git.Oid) string {
	s := oid.String()
	if len(s) >= 7 {
		return s[:7]
	}
	return s
}

// sortedKeys 返回 map 键的稳定排序切片。
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
