package state

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// rec constructs a slog.Record with the given attrs. Time is fixed so
// tests don't need to compare moving values.
func rec(level slog.Level, msg string, attrs ...slog.Attr) slog.Record {
	r := slog.NewRecord(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC), level, msg, 0)
	r.AddAttrs(attrs...)
	return r
}

// TestLiveSink_PerRunDelivery: a record with run_id=A reaches the
// A subscriber and skips the B subscriber.
func TestLiveSink_PerRunDelivery(t *testing.T) {
	ls := NewLiveSink()
	chA, unsubA := ls.Subscribe("A")
	defer unsubA()
	chB, unsubB := ls.Subscribe("B")
	defer unsubB()

	r := rec(slog.LevelInfo, "task started",
		slog.String("entry_type", "task_start"),
		slog.String("run_id", "A"),
	)
	if err := ls.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	select {
	case got := <-chA:
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if m["run_id"] != "A" || m["entry_type"] != "task_start" {
			t.Fatalf("wrong fields: %v", m)
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

// TestLiveSink_NoRunIDDrops: a record with no run_id never reaches any
// subscriber, even the firehose ("" subscriber).
func TestLiveSink_NoRunIDDrops(t *testing.T) {
	ls := NewLiveSink()
	all, unsub := ls.Subscribe("")
	defer unsub()

	r := rec(slog.LevelInfo, "config loaded",
		slog.Int("schedules", 3),
	)
	_ = ls.Handle(context.Background(), r)

	select {
	case got := <-all:
		t.Fatalf("firehose received a no-run_id record: %s", got)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

// TestLiveSink_FirehoseSeesEveryRun: subscribing with runID="" receives
// records from all runs.
func TestLiveSink_FirehoseSeesEveryRun(t *testing.T) {
	ls := NewLiveSink()
	all, unsub := ls.Subscribe("")
	defer unsub()

	for _, id := range []string{"A", "B", "C"} {
		r := rec(slog.LevelInfo, "task started",
			slog.String("entry_type", "task_start"),
			slog.String("run_id", id),
		)
		_ = ls.Handle(context.Background(), r)
	}

	got := make(map[string]bool)
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case b := <-all:
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if id, ok := m["run_id"].(string); ok {
				got[id] = true
			}
		case <-deadline:
			t.Fatalf("firehose missed runs; got %v", got)
		}
	}
}

// TestLiveSink_NoEntryTypeStreamsAnyway: this is the regression that
// proves the architecture switch from Store.publish to LiveSink works.
// A WARN slog with run_id but no entry_type would have been dropped by
// Sink (and therefore never reached the Phase 1 SubscribeEvents path);
// it must stream now.
func TestLiveSink_NoEntryTypeStreamsAnyway(t *testing.T) {
	ls := NewLiveSink()
	ch, unsub := ls.Subscribe("X")
	defer unsub()

	r := rec(slog.LevelWarn, "tool execution failed",
		slog.String("run_id", "X"),
		slog.String("tool", "bash"),
		slog.String("err", "exit 1"),
	)
	_ = ls.Handle(context.Background(), r)

	select {
	case got := <-ch:
		var m map[string]any
		_ = json.Unmarshal(got, &m)
		if m["level"] != "WARN" || m["msg"] != "tool execution failed" {
			t.Fatalf("payload wrong: %v", m)
		}
		if m["entry_type"] != nil {
			t.Fatalf("did not expect entry_type, got: %v", m["entry_type"])
		}
	case <-time.After(time.Second):
		t.Fatal("WARN record did not stream")
	}
}

// TestLiveSink_SlowConsumerDropsNotBlocks: a subscriber that doesn't
// drain its channel must not block other subscribers or future
// Handle calls.
func TestLiveSink_SlowConsumerDropsNotBlocks(t *testing.T) {
	ls := NewLiveSink()
	_, unsubSlow := ls.Subscribe("R") // intentionally drained — exercises drop path
	defer unsubSlow()
	fast, unsubFast := ls.Subscribe("R")
	defer unsubFast()

	// Don't read from `slow`. Push 100 records — buffer is 64, so the
	// rest must drop without blocking.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
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

	// Fast subscriber should have received everything it could buffer
	// (also 64 cap) — but it CAN receive while we drain.
	got := 0
	for {
		select {
		case <-fast:
			got++
			if got >= 64 {
				return // success — fast subscriber kept up to its buffer
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
// records arrive even though the subscription was registered when
// the publish happened.
func TestLiveSink_UnsubscribeStopsDelivery(t *testing.T) {
	ls := NewLiveSink()
	ch, unsub := ls.Subscribe("R")

	// Drain the buffer first, then unsub, then publish.
	unsub()

	r := rec(slog.LevelInfo, "after unsub",
		slog.String("run_id", "R"),
		slog.String("entry_type", "task_start"),
	)
	_ = ls.Handle(context.Background(), r)

	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("expected no delivery after unsub, got: %s", got)
		}
	case <-time.After(100 * time.Millisecond):
		// expected — no message, no close (we documented that abandoning
		// the channel is intentional, see live_sink.go).
	}
}

// TestLiveSink_DoubleUnsubscribeIsSafe: idempotent — calling unsub
// twice doesn't panic.
func TestLiveSink_DoubleUnsubscribeIsSafe(t *testing.T) {
	ls := NewLiveSink()
	_, unsub := ls.Subscribe("R")
	unsub()
	unsub() // would panic if not idempotent
}

// TestLiveSink_InjectMatchesHandleSemantics: Inject takes a pre-encoded
// line and fans out the same way Handle would, used by the ingest path
// (POST /v1/events) where events arrive as bytes from a remote worker
// rather than through this process's slog chain.
func TestLiveSink_InjectMatchesHandleSemantics(t *testing.T) {
	ls := NewLiveSink()
	ch, unsub := ls.Subscribe("R")
	defer unsub()

	line := []byte(`{"time":"2026-05-11T12:00:00Z","level":"INFO","msg":"task started","entry_type":"task_start","run_id":"R","task":"t1"}`)
	ls.Inject("R", line)

	select {
	case got := <-ch:
		if string(got) != string(line) {
			t.Fatalf("payload mismatch: got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Inject did not deliver")
	}

	// Empty runID is a no-op.
	ls.Inject("", line)
}

// TestLiveSink_ConcurrentSubscribePublish: lots of subscribers, lots
// of publishes, race detector must be happy.
func TestLiveSink_ConcurrentSubscribePublish(t *testing.T) {
	ls := NewLiveSink()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsub := ls.Subscribe("R")
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
