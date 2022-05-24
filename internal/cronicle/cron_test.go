package cronicle_test

import (
	"sync"
	"time"

	"testing"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// TestConsumeSchedule calls cronicle.TestConsumeSchedule with three schedules
// each with task.Command = /bin/sleep 2, the schedules should execute concurrently with
// with a total time of < 3 seconds.
func TestConsumeSchedule(t *testing.T) {
	then := time.Now()
	schedule := cronicle.Default().Schedules[0]
	schedule.Tasks[0].Command = []string{"/bin/sleep", "2"}
	schedule.Cron = "@once"
	schedules := []cronicle.Schedule{schedule}
	schedules = append(schedules, schedule)
	schedules = append(schedules, schedule)
	schedules = append(schedules, schedule)

	if len(schedules) == 3 {
		t.Fatalf(`len(schedules) == %d, want 3`, len(schedules))
	}

	var wg sync.WaitGroup
	queue := make(chan []byte)
	go cronicle.ConsumeSchedule(queue, "./", &wg)

	for _, s := range schedules {
		queue <- s.JSON()
	}

	wg.Wait()
	diff := time.Since(then)
	if diff.Seconds() >= 3 {
		t.Fatalf(`time to execute 3 concurrent schedules should be < 3 seconds, is %s`, diff.String())
	}
}
