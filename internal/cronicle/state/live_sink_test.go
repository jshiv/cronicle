package state

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// rec constructs a slog.Record with the given attrs. Time fixed so
// tests don't compare moving values.
func rec(level slog.Level, msg string, attrs ...slog.Attr) slog.Record {
	r := slog.NewRecord(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC), level, msg, 0)
	r.AddAttrs(attrs...)
	return r
}

// trivialEncoder turns a record into "<run_id>:<msg>\n" — easy to
// assert on without importing the cronicle package's pretty handler.
// Includes run_id so firehose tests can distinguish records that share
// the same message.
func trivialEncoder(r slog.Record) []byte {
	var runID string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "run_id" {
			runID = a.Value.String()
			return false
		}
		return true
	})
	return []byte(runID + ":" + r.Message + "\n")
}

// TestLiveSink_PerRunDelivery: a record with run_id=A reaches the
// A subscriber and skips the B subscriber.
func TestLiveSink_PerRunDelivery(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	chA, unsubA := ls.Subscribe(Filter{RunID: "A"})
	defer unsubA()
	chB, unsubB := ls.Subscribe(Filter{RunID: "B"})
	defer unsubB()

	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "hello",
		slog.String("entry_type", "task_start"),
		slog.String("run_id", "A"),
	))

	select {
	case got := <-chA:
		if !strings.Contains(string(got), "hello") {
			t.Fatalf("payload missing message: %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("A did not receive its record")
	}

	select {
	case got := <-chB:
		t.Fatalf("B should not have received A's record: %s", got)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

// TestLiveSink_NoRunIDDrops: a record with no run_id reaches no one —
// process-level lifecycle prints don't belong on per-run SSE.
func TestLiveSink_NoRunIDDrops(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	all, unsub := ls.Subscribe(Filter{})
	defer unsub()

	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "config loaded",
		slog.Int("schedules", 3),
	))

	select {
	case got := <-all:
		t.Fatalf("firehose received a no-run_id record: %s", got)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

// TestLiveSink_FirehoseSeesEveryRun: Subscribe("") receives every run.
func TestLiveSink_FirehoseSeesEveryRun(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	all, unsub := ls.Subscribe(Filter{})
	defer unsub()

	for _, id := range []string{"A", "B", "C"} {
		_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "x",
			slog.String("entry_type", "task_start"),
			slog.String("run_id", id),
		))
	}

	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case b := <-all:
			// Extract run_id prefix from "<run_id>:<msg>\n".
			s := string(b)
			if i := strings.Index(s, ":"); i > 0 {
				got[s[:i]] = true
			}
		case <-deadline:
			t.Fatalf("firehose missed runs; got %v", got)
		}
	}
}

// TestLiveSink_NoEncoderIsNoOp: nil encoder = no-op handler. Useful as
// a placeholder for tests/code paths that don't need live streaming.
func TestLiveSink_NoEncoderIsNoOp(t *testing.T) {
	ls := NewLiveSink(nil)
	ch, unsub := ls.Subscribe(Filter{RunID: "A"})
	defer unsub()

	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "x",
		slog.String("run_id", "A"),
	))

	select {
	case got := <-ch:
		t.Fatalf("nil encoder should drop; got %s", got)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

// TestLiveSink_EmptyEncodedBytesDropped: an encoder returning an empty
// slice (e.g., pretty handler suppressed the record) doesn't ship a
// frame. Avoids stuttering empty events on the wire.
func TestLiveSink_EmptyEncodedBytesDropped(t *testing.T) {
	emptyEnc := func(_ slog.Record) []byte { return nil }
	ls := NewLiveSink(emptyEnc)
	ch, unsub := ls.Subscribe(Filter{RunID: "A"})
	defer unsub()

	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "x",
		slog.String("run_id", "A"),
	))

	select {
	case got := <-ch:
		t.Fatalf("empty encoding should not produce a frame; got %s", got)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

// TestLiveSink_SlowConsumerDropsNotBlocks: a subscriber that doesn't
// drain its channel must not block other subscribers or future Handle
// calls. We push a few-hundred records — the buffer (256) overflows
// and the rest drop without blocking.
func TestLiveSink_SlowConsumerDropsNotBlocks(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	_, unsubSlow := ls.Subscribe(Filter{RunID: "R"}) // intentionally undrained
	defer unsubSlow()
	fast, unsubFast := ls.Subscribe(Filter{RunID: "R"})
	defer unsubFast()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "x",
				slog.String("run_id", "R"),
				slog.Int("seq", i),
			))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("slow subscriber blocked the publisher")
	}

	// Fast subscriber should have received at least 256 (its buffer size).
	got := 0
	for {
		select {
		case <-fast:
			got++
			if got >= 256 {
				return // success
			}
		case <-time.After(200 * time.Millisecond):
			if got == 0 {
				t.Fatalf("fast subscriber got nothing while slow stalled")
			}
			return
		}
	}
}

// TestLiveSink_UnsubscribeStopsDelivery: after unsub, no further
// records arrive even if Handle is called.
func TestLiveSink_UnsubscribeStopsDelivery(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	ch, unsub := ls.Subscribe(Filter{RunID: "R"})
	unsub()

	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "x",
		slog.String("run_id", "R"),
	))

	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("expected no delivery after unsub, got: %s", got)
		}
	case <-time.After(100 * time.Millisecond):
		// expected — channel abandoned, not closed (documented behavior)
	}
}

// TestLiveSink_ScheduleFilterMatchesAnyRunOfSchedule: a subscriber
// with Filter{Schedule: "X"} sees records from every run of schedule
// X — including a NEW run started AFTER subscribe (the chicken-and-egg
// case where a frontend wants to watch the next run before its run_id
// exists).
func TestLiveSink_ScheduleFilterMatchesAnyRunOfSchedule(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	ch, unsub := ls.Subscribe(Filter{Schedule: "X"})
	defer unsub()

	// Record from a different schedule — should NOT match.
	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "ignored",
		slog.String("run_id", "R1"),
		slog.String("schedule", "Y"),
		slog.String("task", "t1"),
	))
	// Two records from schedule X, different run_ids — both should arrive.
	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "first",
		slog.String("run_id", "R1"),
		slog.String("schedule", "X"),
		slog.String("task", "t1"),
	))
	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "second",
		slog.String("run_id", "R2"),
		slog.String("schedule", "X"),
		slog.String("task", "t1"),
	))

	got := []string{}
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case b := <-ch:
			got = append(got, string(b))
		case <-deadline:
			t.Fatalf("got %d frames, want 2: %v", len(got), got)
		}
	}
	// Schedule Y's record must NOT have arrived.
	for _, g := range got {
		if strings.Contains(g, "ignored") {
			t.Fatalf("schedule filter let through wrong-schedule record: %v", got)
		}
	}
}

// TestLiveSink_FilterFirehoseSeesAllSchedules: Filter{} (zero value) is
// the firehose — records from every schedule reach the subscriber.
func TestLiveSink_FilterFirehoseSeesAllSchedules(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	ch, unsub := ls.Subscribe(Filter{})
	defer unsub()

	for _, sched := range []string{"X", "Y", "Z"} {
		_ = ls.Handle(context.Background(), rec(slog.LevelInfo, sched,
			slog.String("run_id", "R-"+sched),
			slog.String("schedule", sched),
			slog.String("task", "t1"),
		))
	}

	got := 0
	deadline := time.After(time.Second)
	for got < 3 {
		select {
		case <-ch:
			got++
		case <-deadline:
			t.Fatalf("firehose got %d, want 3", got)
		}
	}
}

// TestLiveSink_FilterAndedCombined: RunID + Schedule + Task all set
// means all three must match. Only records satisfying every non-empty
// field arrive.
func TestLiveSink_FilterAndedCombined(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	ch, unsub := ls.Subscribe(Filter{RunID: "R1", Schedule: "X", Task: "t1"})
	defer unsub()

	// Wrong task — must NOT arrive.
	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "wrong task",
		slog.String("run_id", "R1"),
		slog.String("schedule", "X"),
		slog.String("task", "t2"),
	))
	// All three match — MUST arrive.
	_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "match",
		slog.String("run_id", "R1"),
		slog.String("schedule", "X"),
		slog.String("task", "t1"),
	))

	select {
	case b := <-ch:
		if !strings.Contains(string(b), "match") {
			t.Fatalf("wrong record passed combined filter: %s", b)
		}
	case <-time.After(time.Second):
		t.Fatal("matching record did not arrive")
	}
	// Confirm nothing else arrives.
	select {
	case b := <-ch:
		t.Fatalf("non-matching record leaked through: %s", b)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

// TestLiveSink_DoubleUnsubscribeIsSafe: idempotent — calling unsub
// twice doesn't panic.
func TestLiveSink_DoubleUnsubscribeIsSafe(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	_, unsub := ls.Subscribe(Filter{RunID: "R"})
	unsub()
	unsub() // would panic if not idempotent
}

// TestLiveSink_InjectFanout: Inject takes pre-encoded bytes and
// delivers them as if they came through Handle. Used by the ingest
// path (POST /v1/events) where worker events arrive as bytes from a
// remote worker rather than through this process's slog chain.
func TestLiveSink_InjectFanout(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)
	ch, unsub := ls.Subscribe(Filter{RunID: "R"})
	defer unsub()

	line := []byte("hello from worker\n")
	ls.Inject("R", "sched", "task1", line)

	select {
	case got := <-ch:
		if string(got) != string(line) {
			t.Fatalf("payload mismatch: got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Inject did not deliver")
	}

	// Empty runID and empty line are no-ops.
	ls.Inject("", "sched", "task1", line)
	ls.Inject("R", "sched", "task1", nil)
}

// TestLiveSink_ConcurrentSubscribePublish: lots of subscribers, lots
// of publishes, race detector must be happy.
func TestLiveSink_ConcurrentSubscribePublish(t *testing.T) {
	ls := NewLiveSink(trivialEncoder)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsub := ls.Subscribe(Filter{RunID: "R"})
			defer unsub()
			go func() {
				for range ch {
				}
			}()
			time.Sleep(10 * time.Millisecond)
		}()
	}
	for i := 0; i < 200; i++ {
		_ = ls.Handle(context.Background(), rec(slog.LevelInfo, "x",
			slog.String("run_id", "R"),
		))
	}
	wg.Wait()
}

// TestTagger_InjectsSeqAndLifetime: a record passing through Tagger
// gets seq + lifetime attrs. Successive records have strictly
// increasing seqs; lifetime is stable for the process.
func TestTagger_InjectsSeqAndLifetime(t *testing.T) {
	collect := &collectingHandler{}
	tagger := NewTagger(collect)
	logger := slog.New(tagger)

	logger.Info("a")
	logger.Info("b")
	logger.Info("c")

	if len(collect.records) != 3 {
		t.Fatalf("want 3 records, got %d", len(collect.records))
	}
	lifetimes := map[string]bool{}
	seqs := []int64{}
	for _, r := range collect.records {
		var lt string
		var seq int64
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "seq":
				seq = a.Value.Int64()
			case "lifetime":
				lt = a.Value.String()
			}
			return true
		})
		lifetimes[lt] = true
		seqs = append(seqs, seq)
	}
	if len(lifetimes) != 1 {
		t.Fatalf("lifetime should be constant; got %d distinct: %v", len(lifetimes), lifetimes)
	}
	if seqs[0] >= seqs[1] || seqs[1] >= seqs[2] {
		t.Fatalf("seqs should be strictly increasing, got %v", seqs)
	}
}

// collectingHandler is a tiny test fake: records every slog.Record
// it receives. No encoding, no filtering.
type collectingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *collectingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (c *collectingHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.records = append(c.records, r)
	c.mu.Unlock()
	return nil
}
func (c *collectingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *collectingHandler) WithGroup(_ string) slog.Handler      { return c }
