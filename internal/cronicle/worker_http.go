package cronicle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// HTTPWorkerOptions configures a worker that consumes via HTTP long-poll
// against a producer's /v1/jobs endpoint instead of via a vice broker.
//
// This is the Phase 2 distributed mode. It replaces the Redis/NSQ
// transport with the producer-served queue, so the only deployment
// dependency is the producer URL.
type HTTPWorkerOptions struct {
	// ProducerURL is the base URL of cronicle run (e.g. http://producer:8765).
	// Required.
	ProducerURL string
	// Token is the bearer credential. Required (the producer refuses to
	// bind /v1/jobs without one).
	Token string
	// WorkerID identifies this consumer in claim/ack. Empty → default
	// "<hostname>-<pid>".
	WorkerID string
	// Path is the local schedule repo root. Used by tasks that have
	// repo blocks; same semantics as the existing StartWorker path
	// argument.
	Path string
	// LogToFile mirrors the producer behavior: when true, structured
	// JSON also writes to .cronicle/log/cronicle.jsonl on the worker
	// host (per-worker audit trail).
	LogToFile bool
	// PollBlock is the long-poll wait passed to /v1/jobs?block=. Default 30s.
	PollBlock time.Duration
	// HeartbeatEvery is the cadence at which a worker pings /heartbeat
	// for its currently-claimed job. Default visibility/3 = ~100s
	// when the producer's visibility is 5 minutes.
	HeartbeatEvery time.Duration
	// PostStateStoreHook fires after the worker's local state store is
	// open and before the long-poll loop starts. Used by the cmd layer
	// to bind the secret-source pump (which writes into state.Backend)
	// without coupling cronicle.StartHTTPWorker to secretsource details.
	PostStateStoreHook func() error
}

// StartHTTPWorker runs the long-poll consumer loop against a remote
// producer. Blocks until ctx is canceled or an unrecoverable transport
// failure occurs (none are unrecoverable today — connection errors trigger
// backoff and retry).
//
// File logging is set up identically to StartWorker — workers running
// distributed get the same on-disk audit trail as single-node.
func StartHTTPWorker(ctx context.Context, opts HTTPWorkerOptions) error {
	if opts.ProducerURL == "" {
		return errors.New("StartHTTPWorker: ProducerURL is required")
	}
	if opts.Token == "" {
		return errors.New("StartHTTPWorker: Token is required (producer refuses unauthenticated workers)")
	}
	if opts.WorkerID == "" {
		opts.WorkerID = defaultWorkerID()
	}
	if opts.PollBlock <= 0 {
		opts.PollBlock = 30 * time.Second
	}
	if opts.HeartbeatEvery <= 0 {
		opts.HeartbeatEvery = 100 * time.Second
	}
	pathAbs, err := filepath.Abs(opts.Path)
	if err != nil {
		return fmt.Errorf("StartHTTPWorker: abs path: %w", err)
	}
	if opts.LogToFile {
		if err := EnableFileLog(pathAbs); err != nil {
			return fmt.Errorf("StartHTTPWorker: file log: %w", err)
		}
	}
	// In-memory projection on the worker side. Events are projected
	// locally too so the worker can answer GET /v1/runs locally if
	// debug-listening, AND so the slog Sink keeps working consistently
	// with single-node mode. We don't ship a worker-side listener in
	// Phase 2b; this is just for symmetry with the producer's slog
	// chain.
	if err := EnableStateStore(":memory:"); err != nil {
		return fmt.Errorf("StartHTTPWorker: state store: %w", err)
	}
	if opts.PostStateStoreHook != nil {
		if err := opts.PostStateStoreHook(); err != nil {
			return fmt.Errorf("StartHTTPWorker: post-state-store hook: %w", err)
		}
	}
	// Ship event records back to the producer so its projection sees
	// what really happened (schedule_start, shell_run, agent_run,
	// schedule_complete). Non-blocking — workers never stall on a
	// slow producer; oldest events drop on overflow.
	shipper := enableEventShipping(opts.ProducerURL, opts.Token)
	if shipper != nil {
		defer shipper.stop()
	}

	hc := &httpQueueClient{
		producerURL: opts.ProducerURL,
		token:       opts.Token,
		workerID:    opts.WorkerID,
		// One full long-poll cycle plus margin. Don't set this too tight
		// or transient retransmits look like timeouts.
		client: &http.Client{Timeout: opts.PollBlock + 30*time.Second},
		ctx:    ctx,
	}
	w := newWorker(ctx, hc, opts.WorkerID, opts.PollBlock, opts.HeartbeatEvery)

	slog.Info("HTTP worker started",
		"worker_id", opts.WorkerID,
		"producer", opts.ProducerURL,
		"poll_block", opts.PollBlock.String(),
	)

	// Control channel: a long-lived SSE connection to the producer for
	// cancel signals (remote-only — see httpQueueClient.controlLoop).
	go hc.controlLoop(w.cancelRun)

	return w.consume(pathAbs)
}

// worker drives one job at a time off a queueClient: claim → execute →
// ack, heartbeating while a job runs. It is transport-agnostic — the same
// loop serves a remote (HTTP) and the in-process (local) self-worker. The
// worker never touches the state store directly; everything goes through
// the queueClient.
type worker struct {
	queue     queueClient
	workerID  string
	pollBlock time.Duration
	hbEvery   time.Duration
	ctx       context.Context
	// activeRuns maps run_id → cancel func of the per-run execute context.
	// The control channel consults this (via cancelRun) when a cancel
	// arrives for a run we hold. Guarded by mu because the cancel path and
	// the execute path touch it concurrently.
	mu         sync.Mutex
	activeRuns map[string]context.CancelFunc
}

func newWorker(ctx context.Context, q queueClient, workerID string, pollBlock, hbEvery time.Duration) *worker {
	if pollBlock <= 0 {
		pollBlock = 30 * time.Second
	}
	if hbEvery <= 0 {
		hbEvery = 100 * time.Second
	}
	return &worker{
		queue:      q,
		workerID:   workerID,
		pollBlock:  pollBlock,
		hbEvery:    hbEvery,
		ctx:        ctx,
		activeRuns: map[string]context.CancelFunc{},
	}
}

// consume runs the claim → execute → ack loop until ctx is canceled. THE
// single worker execution pathway; jobs run one at a time (concurrency is
// a function of worker count, not in-process goroutines).
func (w *worker) consume(pathAbs string) error {
	for {
		select {
		case <-w.ctx.Done():
			return w.ctx.Err()
		default:
		}
		job, gotJob, err := w.queue.Claim(w.pollBlock)
		if err != nil {
			slog.Error("worker claim failed", "error", err.Error())
			// Backoff so a sick queue doesn't hot-loop on errors.
			select {
			case <-w.ctx.Done():
				return w.ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if !gotJob {
			continue
		}
		w.execute(pathAbs, job)
	}
}

func (w *worker) registerRun(runID string, cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.activeRuns == nil {
		w.activeRuns = make(map[string]context.CancelFunc)
	}
	w.activeRuns[runID] = cancel
}

func (w *worker) unregisterRun(runID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.activeRuns, runID)
}

// cancelRun preempts an in-flight run if we hold it. Idempotent. Passed as
// the callback to the (HTTP) control channel.
func (w *worker) cancelRun(runID string) bool {
	w.mu.Lock()
	cancel, ok := w.activeRuns[runID]
	w.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// execute runs the job and acks it. Heartbeats while running.
//
// A per-run execute context carries the cancel signal from the control
// channel: registerRun records the cancel func before the DAG walk; an
// inbound cancel routes through cancelRun(runID). Shell tasks die because
// exec.CommandContext is bound to runCtx; agent tasks check ctx.Done()
// between turns (see pkg/agent).
func (w *worker) execute(croniclePath string, job state.Job) {
	slog.Info("worker claimed job",
		"run_id", job.RunID,
		"schedule", job.Schedule,
		"attempt", job.Attempt,
	)

	runCtx, runCancel := context.WithCancel(w.ctx)
	defer runCancel()
	w.registerRun(job.RunID, runCancel)
	defer w.unregisterRun(job.RunID)

	hbCtx, hbCancel := context.WithCancel(runCtx)
	defer hbCancel()
	go w.heartbeatLoop(hbCtx, job.RunID)

	var sch Schedule
	success := true
	errMsg := ""
	canceled := false
	if err := json.Unmarshal(job.Payload, &sch); err != nil {
		slog.Error("worker: bad payload", "run_id", job.RunID, "error", err.Error())
		success = false
		errMsg = err.Error()
	} else {
		if sch.Source == "" {
			sch.Source = "worker"
		}
		// Plumb the per-run ctx onto the schedule so its task dispatch path
		// (shell exec.CommandContext, agent loop) can honor cancel.
		sch.RunCtx = runCtx
		func() {
			defer func() {
				if r := recover(); r != nil {
					success = false
					errMsg = fmt.Sprintf("panic: %v", r)
				}
			}()
			sch.PropigateTaskProperties(croniclePath)
			sch.ExecuteTasks()
		}()
		// If our run ctx was canceled mid-execute, mark the ack as canceled
		// rather than failed. The projection row already reflects "canceled"
		// from the producer's POST /cancel; this is just the queue record.
		if runCtx.Err() == context.Canceled {
			success = false
			errMsg = "canceled"
			canceled = true
		}
	}

	hbCancel()
	// Don't ack a canceled run as failed — the producer already moved the
	// job row to canceled when it called Cancel(); a subsequent Ack(failed)
	// would be a no-op but it avoids spurious projection writes.
	if canceled {
		return
	}
	if err := w.queue.Ack(job.RunID, success, errMsg); err != nil {
		slog.Error("worker: ack failed", "run_id", job.RunID, "error", err.Error())
	}
}

// heartbeatLoop pings the queue on hbEvery cadence until ctx is canceled.
// A heartbeat failure (e.g. lost ownership) terminates the loop early.
func (w *worker) heartbeatLoop(ctx context.Context, runID string) {
	t := time.NewTicker(w.hbEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.queue.Heartbeat(runID); err != nil {
				slog.Warn("heartbeat failed; stopping heartbeat loop",
					"run_id", runID, "error", err.Error())
				return
			}
		}
	}
}

// defaultWorkerID returns "<hostname>-<pid>" for stable-but-unique
// identification. Operators wanting prettier IDs pass --worker-id.
func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}
