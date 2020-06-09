package pgs

import (
	"container/list"
	"log"
	"time"
)

type TaskManager struct {
	TaskList *list.List
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		TaskList: list.New(),
	}
}

func (tm *TaskManager) Push(t *Task) {
	tm.TaskList.PushBack(t)
}

func (tm *TaskManager) Run() {
	for {
		for e := tm.TaskList.Front(); e != nil; e = e.Next() {
			task := e.Value.(*Task)
			switch task.Status {
			case TSOpen:
				if task.Ready() {
					log.Printf("Task %s start", task.Id)
					go func() {
						err := task.Process()
						if err != nil {
							task.Status = TSFailed
						} else {
							task.Status = TSFinished
						}
					}()
				}
			case TSFailed:
				if task.OnFailed != nil {
					task.OnFailed(task, nil)
				}
			case TSFinished:
				if task.OnFinished != nil {
					task.OnFinished(task, nil)
				}
				log.Printf("Task %s has finished, remove", task.Id)
				tm.TaskList.Remove(e)
			}
		}
		time.Sleep(1 * time.Second)
	}
}