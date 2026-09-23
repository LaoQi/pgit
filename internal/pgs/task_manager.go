package pgs

import (
	"container/list"
	"log"
	"sync"
	"time"
)

// TaskManager 单 goroutine 调度循环 + 每任务一个执行 goroutine。
// TaskList 由 mu 保护（Push 可来自其他 goroutine）。
type TaskManager struct {
	TaskList *list.List
	mu       sync.Mutex
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		TaskList: list.New(),
	}
}

func (tm *TaskManager) Push(t *Task) {
	tm.mu.Lock()
	tm.TaskList.PushBack(t)
	tm.mu.Unlock()
}

func (tm *TaskManager) Run() {
	for {
		tm.mu.Lock()
		for e := tm.TaskList.Front(); e != nil; {
			next := e.Next()
			task := e.Value.(*Task)
			switch task.GetStatus() {
			case TSOpen:
				if task.Ready() {
					// 启动前置为 Running，避免下一轮把同一任务重复派发
					task.SetStatus(TSRunning)
					log.Printf("Task %s start", task.Id)
					go func() {
						if err := task.Process(); err != nil {
							task.SetStatus(TSFailed)
						} else {
							task.SetStatus(TSFinished)
						}
					}()
				}
			case TSFailed:
				if task.OnFailed != nil {
					task.OnFailed(task, nil)
				}
				log.Printf("Task %s has failed, remove", task.Id)
				tm.TaskList.Remove(e)
			case TSFinished:
				if task.OnFinished != nil {
					task.OnFinished(task, nil)
				}
				log.Printf("Task %s has finished, remove", task.Id)
				tm.TaskList.Remove(e)
			}
			e = next
		}
		tm.mu.Unlock()
		time.Sleep(1 * time.Second)
	}
}
