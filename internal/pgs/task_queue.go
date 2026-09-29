package pgs

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// 有界并发任务队列：固定容量 + 可热调整的 worker 数。
//
// 语义刻意从简（与镜像同步的实际需求对齐）：
//   - 容量固定，队列满时 Submit 直接失败（由调用方决定丢弃还是报错），不阻塞、不排队等待；
//   - 排队中的任务不做公平性保证，长任务可以一直占着 worker（「饿死」可接受）；
//   - 单任务不设执行超时，执行时长由任务自身（如 fetch 的停滞超时/重试）兜住；
//   - Stop 丢弃未开始的任务（对应任务的 Dropped 回调会被调用）并等待运行中的任务收尾。
type TaskQueue struct {
	capacity int

	tasks chan *QueueTask
	done  chan struct{}

	mu      sync.Mutex
	workers map[chan struct{}]struct{} // 每个 worker 的退休信号
	running int
	closed  bool

	completed atomic.Uint64
	dropped   atomic.Uint64
	wg        sync.WaitGroup
}

// QueueTask 是一条待执行任务。Run 与 Dropped 可为 nil。
type QueueTask struct {
	ID string
	// Run 是任务体，在 worker goroutine 中执行（panic 会被 recover 并记日志）。
	Run func()
	// Done 在任务执行结束（或被丢弃前的正常结束）后关闭；用于同步等待。
	Done chan struct{}
	// Dropped 在任务未被执行就丢弃时回调（队列满 / Stop 丢弃未开始的队列），
	// 用于让等待方及时醒来，而不是干等到超时。
	Dropped func()
}

// QueueStats 是队列的瞬时视图。
type QueueStats struct {
	Capacity  int    `json:"capacity"`
	Workers   int    `json:"workers"`
	Queued    int    `json:"queued"`
	Running   int    `json:"running"`
	Completed uint64 `json:"completed"`
	Dropped   uint64 `json:"dropped"`
	Closed    bool   `json:"closed"`
}

// NewTaskQueue 构造队列；workers<=0 时至少 1 个，capacity<=0 时至少 1。
func NewTaskQueue(capacity, workers int) *TaskQueue {
	if capacity < 1 {
		capacity = 1
	}
	if workers < 1 {
		workers = 1
	}
	q := &TaskQueue{
		capacity: capacity,
		tasks:    make(chan *QueueTask, capacity),
		done:     make(chan struct{}),
		workers:  make(map[chan struct{}]struct{}),
	}
	q.mu.Lock()
	for i := 0; i < workers; i++ {
		q.spawnLocked()
	}
	q.mu.Unlock()
	return q
}

// spawnLocked 启动一个 worker（调用方须持有 q.mu）。
func (q *TaskQueue) spawnLocked() {
	retire := make(chan struct{})
	q.workers[retire] = struct{}{}
	q.wg.Add(1)
	go q.worker(retire)
}

func (q *TaskQueue) worker(retire chan struct{}) {
	defer q.wg.Done()
	for {
		// 先看退休/停止信号，避免 SetWorkers 收缩后仍继续抢任务。
		select {
		case <-retire:
			return
		case <-q.done:
			return
		default:
		}
		select {
		case <-retire:
			return
		case <-q.done:
			return
		case t := <-q.tasks:
			q.run(t)
		}
	}
}

func (q *TaskQueue) run(t *QueueTask) {
	q.mu.Lock()
	q.running++
	q.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("task queue: task panic", "task", t.ID, "panic", r)
		}
		q.mu.Lock()
		q.running--
		q.mu.Unlock()
		q.completed.Add(1)
		if t.Done != nil {
			close(t.Done)
		}
	}()
	if t.Run != nil {
		t.Run()
	}
}

// Submit 尝试入队。队列满返回 ErrQueueFull，已停止返回 ErrQueueClosed；
// 两种情况都会递增 dropped 并调用任务的 Dropped 回调。
func (q *TaskQueue) Submit(t *QueueTask) error {
	if t == nil {
		return ErrQueueClosed
	}
	q.mu.Lock()
	closed := q.closed
	q.mu.Unlock()
	if closed {
		q.dropped.Add(1)
		if t.Dropped != nil {
			t.Dropped()
		}
		return ErrQueueClosed
	}
	select {
	case q.tasks <- t:
		return nil
	default:
		q.dropped.Add(1)
		if t.Dropped != nil {
			t.Dropped()
		}
		return ErrQueueFull
	}
}

// SetWorkers 调整 worker 数（>=1）。扩容立即生效；收缩时让多余 worker 跑完
// 当前任务后自行退出（极端情况下可能多跑一个任务）。已停止的队列不再调整。
func (q *TaskQueue) SetWorkers(n int) {
	if n < 1 {
		n = 1
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	cur := len(q.workers)
	switch {
	case n == cur:
		return
	case n > cur:
		for i := cur; i < n; i++ {
			q.spawnLocked()
		}
	default:
		excess := cur - n
		for retire := range q.workers {
			if excess == 0 {
				break
			}
			close(retire)
			delete(q.workers, retire)
			excess--
		}
	}
}

// Stats 返回队列瞬时状态。Workers 为期望的 worker 数；收缩期间正在收尾的
// worker 已从统计中移除（可能短暂大于实际在跑的 worker 数）。
func (q *TaskQueue) Stats() QueueStats {
	q.mu.Lock()
	workers, running, closed := len(q.workers), q.running, q.closed
	q.mu.Unlock()
	return QueueStats{
		Capacity:  q.capacity,
		Workers:   workers,
		Queued:    len(q.tasks),
		Running:   running,
		Completed: q.completed.Load(),
		Dropped:   q.dropped.Load(),
		Closed:    closed,
	}
}

// Stop 停止队列：不再接收新任务，丢弃未开始的任务（触发其 Dropped 回调），
// 等待运行中的任务收尾。可重复调用。
func (q *TaskQueue) Stop() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.done)
	}
	q.workers = make(map[chan struct{}]struct{})
	q.mu.Unlock()

	q.wg.Wait()

drain:
	for {
		select {
		case t := <-q.tasks:
			q.dropped.Add(1)
			if t.Dropped != nil {
				t.Dropped()
			}
		default:
			break drain
		}
	}
}
