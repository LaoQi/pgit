package pgs

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitClosed(t *testing.T, ch <-chan struct{}, d time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatal("timeout waiting for task completion")
	}
}

// 并发度不超过配置的 worker 数，且确实并行执行（>1）。
func TestTaskQueueConcurrencyLimit(t *testing.T) {
	q := NewTaskQueue(64, 3)
	defer q.Stop()

	var running, peak int64
	var wg sync.WaitGroup
	done := make(chan struct{}, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		d := make(chan struct{})
		if err := q.Submit(&QueueTask{ID: "t", Run: func() {
			defer wg.Done()
			cur := atomic.AddInt64(&running, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt64(&running, -1)
			done <- struct{}{}
		}, Done: d}); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	for i := 0; i < 12; i++ {
		<-done
	}
	wg.Wait()
	if got := atomic.LoadInt64(&peak); got > 3 {
		t.Errorf("peak concurrency = %d, want <= 3", got)
	}
	if got := atomic.LoadInt64(&peak); got < 2 {
		t.Errorf("peak concurrency = %d, want >= 2 (workers should run in parallel)", got)
	}
	st := q.Stats()
	if st.Workers != 3 || st.Completed != 12 || st.Dropped != 0 || st.Queued != 0 || st.Running != 0 {
		t.Errorf("stats = %+v, want workers=3 completed=12 queued=0 running=0", st)
	}
}

// SetWorkers 扩容/收缩生效，收缩后并发度下降且队列仍可工作。
func TestTaskQueueSetWorkers(t *testing.T) {
	q := NewTaskQueue(64, 1)
	defer q.Stop()

	q.SetWorkers(4)
	if got := q.Stats().Workers; got != 4 {
		t.Fatalf("workers = %d, want 4", got)
	}

	var peak int64
	var running int64
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(4)
	for i := 0; i < 4; i++ {
		_ = q.Submit(&QueueTask{Run: func() {
			cur := atomic.AddInt64(&running, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
					break
				}
			}
			started.Done()
			<-release
			atomic.AddInt64(&running, -1)
		}})
	}
	waitAll(t, &started, 2*time.Second)
	if got := atomic.LoadInt64(&peak); got != 4 {
		t.Fatalf("peak = %d, want 4 before shrink", got)
	}

	// 收缩到 1：等 4 个占用者退出后按新并发度运行
	q.SetWorkers(1)
	if got := q.Stats().Workers; got != 1 {
		t.Fatalf("workers after shrink = %d, want 1", got)
	}
	close(release)
	// 等这批任务全部退出（peak 归零由 running 计数体现）
	waitCond(t, func() bool { return atomic.LoadInt64(&running) == 0 }, 2*time.Second)

	atomic.StoreInt64(&peak, 0)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		_ = q.Submit(&QueueTask{Run: func() {
			defer wg.Done()
			cur := atomic.AddInt64(&running, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt64(&running, -1)
		}})
	}
	wg.Wait()
	if got := atomic.LoadInt64(&peak); got > 1 {
		t.Errorf("peak after shrink = %d, want <= 1", got)
	}
}

// 队列满：Submit 返回 ErrQueueFull，dropped 计数与 Dropped 回调都触发。
func TestTaskQueueFullDrops(t *testing.T) {
	q := NewTaskQueue(2, 1)
	defer q.Stop()

	release := make(chan struct{})
	started := make(chan struct{})
	if err := q.Submit(&QueueTask{Run: func() { close(started); <-release }}); err != nil {
		t.Fatal(err)
	}
	<-started
	// 占满容量
	if err := q.Submit(&QueueTask{Run: func() {}}); err != nil {
		t.Fatal(err)
	}
	if err := q.Submit(&QueueTask{Run: func() {}}); err != nil {
		t.Fatal(err)
	}

	var dropped int32
	err := q.Submit(&QueueTask{Run: func() {}, Dropped: func() { atomic.AddInt32(&dropped, 1) }})
	if err != ErrQueueFull {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	if atomic.LoadInt32(&dropped) != 1 {
		t.Error("Dropped callback should be called when queue is full")
	}
	if got := q.Stats().Dropped; got != 1 {
		t.Errorf("dropped = %d, want 1", got)
	}
	close(release)
}

// Stop：丢弃未开始任务（回调 + dropped），等待运行中任务收尾，幂等，之后 Submit 拒绝。
func TestTaskQueueStop(t *testing.T) {
	q := NewTaskQueue(8, 1)

	release := make(chan struct{})
	started := make(chan struct{})
	runningDone := make(chan struct{})
	if err := q.Submit(&QueueTask{Run: func() { close(started); <-release; close(runningDone) }}); err != nil {
		t.Fatal(err)
	}
	<-started

	var dropped int32
	for i := 0; i < 3; i++ {
		if err := q.Submit(&QueueTask{Run: func() {}, Dropped: func() { atomic.AddInt32(&dropped, 1) }}); err != nil {
			t.Fatal(err)
		}
	}

	stopped := make(chan struct{})
	go func() { q.Stop(); close(stopped) }()

	// Stop 不能提前返回：运行中的任务还没结束
	select {
	case <-stopped:
		t.Fatal("Stop returned before running task finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-runningDone
	waitClosed(t, stopped, 2*time.Second)

	if got := atomic.LoadInt32(&dropped); got != 3 {
		t.Errorf("dropped callbacks = %d, want 3", got)
	}
	st := q.Stats()
	if !st.Closed || st.Queued != 0 {
		t.Errorf("stats = %+v, want closed with empty queue", st)
	}
	if err := q.Submit(&QueueTask{}); err != ErrQueueClosed {
		t.Errorf("submit after stop = %v, want ErrQueueClosed", err)
	}
	q.Stop() // 幂等
}

// 任务 panic 不影响队列继续工作，Done 仍会关闭。
func TestTaskQueuePanicRecovered(t *testing.T) {
	q := NewTaskQueue(4, 1)
	defer q.Stop()

	done := make(chan struct{})
	if err := q.Submit(&QueueTask{ID: "boom", Run: func() { panic("boom") }, Done: done}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, done, 2*time.Second)

	ok := make(chan struct{})
	if err := q.Submit(&QueueTask{Run: func() { close(ok) }}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, ok, 2*time.Second)
}

func waitAll(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	waitClosed(t, ch, d)
}

func waitCond(t *testing.T, cond func() bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout waiting for condition")
}
