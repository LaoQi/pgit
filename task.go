package main

type Context struct {
}

type Task struct {
	ID      string
	context Context
}

type TaskManager struct {
	tasks []*Task
}

func (tm *TaskManager) NewTask() {

}
