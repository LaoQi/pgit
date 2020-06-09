package pgs

import (
	"fmt"
	"log"
	"testing"
	"time"
)

func NewProcessor(index int) ProcessorFunc  {
	return func(task *Task) error {
		log.Printf("I am task %d", index)
		time.Sleep(1)
		return nil
	}
}

func TestTaskManager(t *testing.T) {
	tm := NewTaskManager()

	for i := range []int{0, 1, 2, 3, 4}{
		tm.Push(&Task{
			Id:        fmt.Sprintf("t%d", i),
			Type:      TP_Default,
			Cron:      time.Now().Add(time.Duration(i) * time.Second),
			Status:    0,
			Processor: NewProcessor(i),
		})
	}

	go tm.Run()
	time.Sleep(5 * time.Second)
	t.Log("tm run over")
}
