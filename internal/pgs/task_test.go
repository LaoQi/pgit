package pgs

import (
	"fmt"
	"log"
	"testing"
	"time"
)

func NewProcessor(index int) ProcessorFunc {
	return func(task *Task) error {
		log.Printf("I am task %d", index)
		time.Sleep(1 * time.Second)
		return nil
	}
}

func TestTaskManager(t *testing.T) {
	tm := NewTaskManager()

	for i := range []int{0, 1, 2, 3, 4} {
		tm.Push(&Task{
			Id:        fmt.Sprintf("t%d", i),
			Type:      TP_Default,
			Cron:      time.Now().Add(time.Duration(i) * time.Second),
			Processor: NewProcessor(i),
		})
	}

	go tm.Run()
	time.Sleep(6 * time.Second)
	t.Log("tm run over")

	// 任务调度完结后应被移除（轮询等待，避免与 1s 调度间隔竞速）
	deadline := time.Now().Add(5 * time.Second)
	for {
		tm.mu.Lock()
		n := tm.TaskList.Len()
		tm.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task list len = %d, want 0 (finished tasks must be removed)", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
