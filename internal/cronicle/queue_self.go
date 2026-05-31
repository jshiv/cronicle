package cronicle

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
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

// selfWorker is the in-process worker for single-node mode. Rather than
// claiming from the state store directly, it drives jobs through the SAME
// HTTP pathway a remote worker uses — long-polling the producer's own
// /v1/jobs over loopback, executing, and acking over HTTP. Net effect:
// ONE worker code path (no in-process special case), and the worker never
// holds a store handle, so the producer stays the sole owner of state.
//
// It is only started when the listener is configured (see Run), so the
// loopback endpoint is always reachable; consume() backs off and retries
// until it binds. Unlike a remote worker it does NOT ship run events over
// HTTP — they already reach the store through the producer's own in-process
// slog sink (same process); shipping too would double-apply them.
//
// One difference worth calling out: jobs run one at a time (consume is
// serial), matching the remote worker. Single-node concurrency is now a
// function of how many self-workers you run, not in-process goroutines.
func selfWorker(ctx context.Context, producerURL, token, croniclePath string, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()
	w := newHTTPWorker(HTTPWorkerOptions{
		ProducerURL: producerURL,
		Token:       token,
		WorkerID:    SelfWorkerID(),
		Path:        croniclePath,
	}, ctx)
	slog.Info("self-worker started (http loopback)",
		"worker_id", w.opts.WorkerID, "producer", producerURL)
	if err := w.consume(croniclePath); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("self-worker stopped", "error", err.Error())
	}
}

// loopbackURL turns the listener bind address (":8765", "0.0.0.0:8765")
// into a URL the in-process self-worker can dial on loopback.
func loopbackURL(listenAddr string) string {
	if _, port, err := net.SplitHostPort(listenAddr); err == nil && port != "" {
		return "http://127.0.0.1:" + port
	}
	return "http://" + listenAddr
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
