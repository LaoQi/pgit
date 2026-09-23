package pgs

import (
	"fmt"
	"sync"
	"time"
)

type TaskStatus int

const (
	TSOpen = iota
	TSRunning
	TSFailed
	TSFinished
)

type TaskType uint16
type ProcessorFunc func(t *Task) error
type StateFunc func(t *Task, e *TaskEvent)

const (
	TP_Default = iota
)

type TaskRunnable interface {
	Process() error
}

type TaskEvent struct{}

// Task 任务定义。status 由 TaskManager 的调度循环与执行 goroutine 并发访问，
// 统一通过 GetStatus/SetStatus 读写。
type Task struct {
	Id        string
	Type      TaskType
	Cron      time.Time
	Processor ProcessorFunc

	OnStart    StateFunc
	OnFailed   StateFunc
	OnFinished StateFunc

	mu     sync.Mutex
	status TaskStatus
}

// GetStatus 返回任务当前状态（并发安全）。
func (t *Task) GetStatus() TaskStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// SetStatus 设置任务状态（并发安全）。
func (t *Task) SetStatus(s TaskStatus) {
	t.mu.Lock()
	t.status = s
	t.mu.Unlock()
}

func (t *Task) Ready() bool {
	return t.Cron.Before(time.Now())
}

func (t *Task) Process() error {
	if t.Processor != nil {
		t.SetStatus(TSRunning)
		return t.Processor(t)
	}
	panic(fmt.Errorf("task %s processor is nil", t.Id))
}
