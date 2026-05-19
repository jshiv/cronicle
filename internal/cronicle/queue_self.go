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
			payload, _ = json.Marshal(sch)
		}
		if err := store.Enqueue(sch.RunID, sch.Name, payload); err != nil {
			slog.Error("self-queue: enqueue failed", "run_id", sch.RunID, "error", err.Error())
		}
	}
}

// selfWorker is the in-process consumer for --queue self mode. Claims
// jobs from the SQLite store, hands the payload to the same DAG walker
// the existing ConsumeSchedule path uses, then acks. Treats Apply
// failures as "the run executed but recorded a schedule_complete with
// success=false" — the job is acked DONE either way; the projection
// has the failure detail.
//
// Visibility timeout is 5 minutes. Long-running agent runs aren't
// covered by that without explicit Heartbeat — single-node mode
// rarely needs it because the worker IS the producer (no network
// partition can preempt), but if a panic kills the goroutine the
// reaper will recover the job.
func selfWorker(ctx context.Context, store state.Backend, croniclePath string, wg *sync.WaitGroup) {
	workerID := SelfWorkerID()
	slog.Info("self-worker started", "worker_id", workerID)

	for {
		// Bail before each Claim attempt so a cancel signal observed
		// between WaitForJob returning and the next Claim still ends
		// the loop promptly.
		if ctx.Err() != nil {
			slog.Info("self-worker: shutting down", "worker_id", workerID)
			return
		}
		job, err := store.Claim(workerID, 5*time.Minute)
		if errors.Is(err, state.ErrNoJobs) {
			// Park until a wakeup or 30s elapses, then re-Claim. Passing
			// ctx here lets cancellation interrupt the park immediately
			// instead of waiting for the 30s ceiling.
			store.WaitForJob(ctx, 30*time.Second)
			continue
		}
		if err != nil {
			slog.Error("self-worker: claim failed", "error", err.Error())
			// Sleep through cancel — return early on ctx.Done so shutdown
			// isn't blocked by the back-off.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		wg.Add(1)
		go func(j state.Job) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("self-worker: panic during execute",
						"run_id", j.RunID, "panic", r)
					_ = store.Ack(j.RunID, workerID, false, "panic during execute")
				}
			}()

			var sch Schedule
			if err := json.Unmarshal(j.Payload, &sch); err != nil {
				slog.Error("self-worker: payload decode failed",
					"run_id", j.RunID, "error", err.Error())
				_ = store.Ack(j.RunID, workerID, false, err.Error())
				return
			}
			sch.PropigateTaskProperties(croniclePath)
			sch.ExecuteTasks()
			// Per-task success rolls into the projection via the slog
			// Sink; the queue Ack just records "this worker handled
			// this job to completion." Run-level success is recorded
			// separately on the projection's runs row.
			_ = store.Ack(j.RunID, workerID, true, "")
		}(job)
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
