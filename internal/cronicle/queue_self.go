package cronicle

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// enqueueAdapter drains the cron-tick / listener-trigger channel and
// pushes each Schedule JSON into the SQLite jobs table for durable,
// claim-based dispatch. Decode errors are logged-and-dropped — the
// schedule never executes if it can't be parsed for queueing, which
// is the same outcome as today (json.Unmarshal failure inside
// ConsumeSchedule swallows the schedule).
//
// The channel buffer (caller-sized) lets the cron tick return quickly
// even if SQLite is briefly busy. At our load (sub-100 events/min)
// the buffer is essentially never used, but it bounds the worst-case
// blocking on the cron goroutine.
func enqueueAdapter(in <-chan []byte, store state.Backend) {
	for payload := range in {
		var sch Schedule
		if err := json.Unmarshal(payload, &sch); err != nil {
			slog.Error("self-queue: bad payload, dropping", "error", err.Error())
			continue
		}
		if sch.RunID == "" {
			// Should never happen: ProduceSchedule and the listener both
			// stamp RunID before queueing. Defensive: synthesize one
			// rather than dropping the run.
			sch.RunID = newRunID()
		}
		// Resolve ${last_run} HERE, on the producer, against the
		// authoritative state store, and bake it into the payload. The
		// worker that later claims this job (single-node self-worker or a
		// distributed HTTP worker) executes from the fully-resolved payload
		// and never reads state itself — its local store is a
		// non-authoritative in-memory projection with no run history.
		resolveLastRun(store, &sch)
		payload, _ = json.Marshal(sch)
		if err := store.Enqueue(sch.RunID, sch.Name, payload); err != nil {
			slog.Error("self-queue: enqueue failed", "run_id", sch.RunID, "error", err.Error())
		}
	}
}

// selfWorker is the in-process worker for single-node mode (--worker). It
// runs the SAME claim→execute→ack loop as a remote worker (see worker /
// consume), but over a localQueueClient — a thin in-process adapter on the
// state backend, with NO socket. The worker's only contact with state is
// the three queue verbs (Claim/Ack/Heartbeat); it never reads run history
// or projections, so the producer stays the single source of truth by
// construction, not convention.
//
// Run events still reach the store through the producer's in-process slog
// sink (same process), so unlike a remote worker there is nothing to ship.
// Jobs run one at a time (consume is serial); the reaper recovers a job if
// this worker dies mid-run.
func selfWorker(ctx context.Context, store state.Backend, croniclePath string, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()
	workerID := SelfWorkerID()
	lc := &localQueueClient{store: store, workerID: workerID, ctx: ctx}
	w := newWorker(ctx, lc, workerID, 30*time.Second, 100*time.Second)
	slog.Info("self-worker started", "worker_id", workerID)
	if err := w.consume(croniclePath); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("self-worker stopped", "error", err.Error())
	}
}

// reaperLoop periodically walks the jobs table for claims past their
// visibility deadline and moves them back to pending. Cadence is 10s —
// shorter than the smallest reasonable visibility (we use 5 minutes)
// by 30×, so a worker death is recovered within 5m+10s at worst.
//
// Logs only when something is actually reaped — quiet when the queue
// is healthy.
func reaperLoop(ctx context.Context, store state.Backend) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			moved, err := store.ReapExpired()
			if err != nil {
				slog.Error("reaper: ReapExpired failed", "error", err.Error())
				continue
			}
			if moved > 0 {
				slog.Warn("reaper: moved expired claims back to pending", "count", moved)
			}
		}
	}
}

// SelfWorkerID generates a stable id for the in-process consumer:
// "self-<hostname>-<pid>". External workers use a separate id derived
// from --worker-id (or the same hostname-pid fallback).
func SelfWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return "self-" + host + "-" + strconv.Itoa(os.Getpid())
}
