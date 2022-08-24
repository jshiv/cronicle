package cronicle_test

import (
	"log"
	"sync"
	"time"

	"testing"

	"github.com/jshiv/cronicle/internal/cronicle"

	driver "github.com/faabiosr/cachego/sync"

	rd "gopkg.in/redis.v4"

	"github.com/faabiosr/cachego/redis"

	bolt "go.etcd.io/bbolt"

	cachegoBolt "github.com/faabiosr/cachego/bolt"
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

//TODO write remote schedule cache to enable better retry logic like "wait until prior execution is finished"
func TestCache(t *testing.T) {
	then := time.Now()
	cache := driver.New()
	if err := cache.Save("user_id", "1", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	id, err := cache.Fetch("user_id")
	if err != nil {
		t.Fatal(err)
	}

	log.Printf("user id: %s \n", id)
	diff := time.Since(then)
	if diff.Seconds() >= 3 {
		t.Fatalf(`time to execute 3 concurrent schedules should be < 3 seconds, is %s`, diff.String())
	}
}

func TestRedisCache(t *testing.T) {
	then := time.Now()
	cache := redis.New(
		rd.NewClient(&rd.Options{
			Addr: ":6379",
		}),
	)

	if err := cache.Save("user_id", "1", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	id, err := cache.Fetch("user_id")
	if err != nil {
		t.Fatal(err)
	}

	println("user id: %s \n", id)
	diff := time.Since(then)
	if diff.Seconds() >= 3 {
		t.Fatalf(`time to execute 3 concurrent schedules should be < 3 seconds, is %s`, diff.String())
	}
}

func TestBoltCache(t *testing.T) {
	then := time.Now()

	db, err := bolt.Open("cache.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cache := cachegoBolt.New(db)
	bolt.

	if err := cache.Save("user_id", "1", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	id, err := cache.Fetch("user_id")
	if err != nil {
		t.Fatal(err)
	}

	println("user id: %s \n", id)
	diff := time.Since(then)
	if diff.Seconds() >= 3 {
		t.Fatalf(`time to execute 3 concurrent schedules should be < 3 seconds, is %s`, diff.String())
	}
}

//TODO: Add remote caching to check for task status for wait/retry/skip logic
// As well as dependency across schedules logic.
func TestGroupCache(t *testing.T){
	//https://github.com/golang/groupcache
	// could be good 
}