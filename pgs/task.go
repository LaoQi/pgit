package pgs

import (
	"fmt"
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

type TaskEvent struct {

}

type Task struct {
	Id string
	Type TaskType
	Cron time.Time
	Status TaskStatus
	Processor ProcessorFunc

	OnStart StateFunc
	OnFailed StateFunc
	OnFinished StateFunc
}

func (t *Task) Ready() bool {
	return t.Cron.Before(time.Now())
}

func (t *Task) Process() error {
	if t.Processor != nil {
		t.Status = TSRunning
		return t.Processor(t)
	}
	panic(fmt.Errorf("Task %s processor is nil", t.Id))
}

