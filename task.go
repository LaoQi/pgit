package main

import "log"

type TaskType int

const (
	TaskType_CheckCommits = iota
	TaskType_WebHook
	TaskType_Rsync
)

type Context struct {
}

type TaskBase struct {
	ID       string
	taskType TaskType
	context  Context
}

type TaskImp interface {
	Process() error
}

type TaskManager struct {
	tasks chan *TaskImp
}

func (tm *TaskManager) NewTask(task *TaskImp) {
	tm.tasks <- task
}

func (tm *TaskManager) Start() error {
	log.Printf("Start TaskManager")
	// for task := range tm.tasks {

	// }
	return nil
}
