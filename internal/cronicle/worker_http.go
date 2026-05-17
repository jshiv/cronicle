package cronicle

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// StartHTTPWorker runs the long-poll consumer loop. Blocks until ctx
// is canceled or an unrecoverable transport failure occurs (none are
// unrecoverable today — connection errors trigger backoff and retry).
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

	client := &http.Client{
		// One full long-poll cycle plus margin. Don't set this too
		// tight or transient retransmits look like timeouts.
		Timeout: opts.PollBlock + 30*time.Second,
	}
	w := &httpWorker{
		opts:   opts,
		client: client,
		ctx:    ctx,
	}

	slog.Info("HTTP worker started",
		"worker_id", opts.WorkerID,
		"producer", opts.ProducerURL,
		"poll_block", opts.PollBlock.String(),
	)

	// Control channel: a long-lived SSE connection to the producer
	// for cancel signals. The goroutine reconnects on disconnects so
	// transient producer restarts don't permanently disable cancel.
	go w.controlLoop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		job, gotJob, err := w.pollOnce()
		if err != nil {
			slog.Error("worker poll failed", "error", err.Error())
			// Backoff. With the producer down workers should not
			// hot-loop on connection errors.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if !gotJob {
			// 204 No Content — the long-poll waited and got nothing.
			// Just reconnect immediately.
			continue
		}
		w.execute(pathAbs, job)
	}
}

type httpWorker struct {
	opts   HTTPWorkerOptions
	client *http.Client
	ctx    context.Context
	// activeRuns maps run_id → cancel func of the per-run execute
	// context. The control-channel goroutine consults this when a
	// cancel SSE message arrives for a run we hold. Guarded by a
	// mutex because the SSE goroutine and the execute goroutine touch
	// it concurrently.
	mu         sync.Mutex
	activeRuns map[string]context.CancelFunc
}

// registerRun records that we've started executing run_id with the
// given cancel func. Caller calls unregisterRun via defer.
func (w *httpWorker) registerRun(runID string, cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.activeRuns == nil {
		w.activeRuns = make(map[string]context.CancelFunc)
	}
	w.activeRuns[runID] = cancel
}

func (w *httpWorker) unregisterRun(runID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.activeRuns, runID)
}

// cancelRun preempts an in-flight run if we hold it. Idempotent —
// calling cancel() on a context multiple times is safe.
func (w *httpWorker) cancelRun(runID string) bool {
	w.mu.Lock()
	cancel, ok := w.activeRuns[runID]
	w.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

func (w *httpWorker) authHeader() string { return "Bearer " + w.opts.Token }

// pollOnce issues one GET /v1/jobs roundtrip. Returns (job, true, nil)
// on a 200 with a job, (zero, false, nil) on a 204, and (zero, false, err)
// on transport / decode errors.
func (w *httpWorker) pollOnce() (state.Job, bool, error) {
	url := strings.TrimRight(w.opts.ProducerURL, "/") +
		"/v1/jobs?worker=" + w.opts.WorkerID +
		"&block=" + w.opts.PollBlock.String()
	req, err := http.NewRequestWithContext(w.ctx, http.MethodGet, url, nil)
	if err != nil {
		return state.Job{}, false, err
	}
	req.Header.Set("Authorization", w.authHeader())
	resp, err := w.client.Do(req)
	if err != nil {
		return state.Job{}, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return state.Job{}, false, nil
	case http.StatusOK:
		var j state.Job
		if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
			return state.Job{}, false, fmt.Errorf("decode job: %w", err)
		}
		return j, true, nil
	default:
		body, _ := io.ReadAll(resp.Body)
		return state.Job{}, false, fmt.Errorf("claim http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// execute runs the job and acks it. Heartbeats while running.
//
// Phase 3: a per-run execute context carries the cancel signal from
// the SSE control channel. Workers register their cancel func before
// starting the DAG walk; an inbound cancel SSE message routes through
// w.cancelRun(runID) which calls cancel() on the matching context.
// Shell tasks die because exec.CommandContext is bound to runCtx;
// agent tasks check ctx.Done() between turns (see pkg/agent).
func (w *httpWorker) execute(croniclePath string, job state.Job) {
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
		// Plumb the per-run ctx onto the schedule so its task
		// dispatch path (shell exec.CommandContext, agent loop) can
		// honor cancel.
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
		// If our run ctx was canceled mid-execute, mark the ack as
		// canceled rather than failed. The projection row already
		// reflects "canceled" from the producer's POST /cancel; this
		// is just the queue-side record.
		if runCtx.Err() == context.Canceled {
			success = false
			errMsg = "canceled"
			canceled = true
		}
	}

	hbCancel()
	// Don't ack a canceled run as failed — the producer already moved
	// the job row to canceled when it called Cancel(); a subsequent
	// Ack(failed) would be a no-op (UPDATE with WHERE status=claimed
	// matches nothing) but it avoids spurious projection writes.
	if canceled {
		return
	}
	if err := w.ack(job.RunID, success, errMsg); err != nil {
		slog.Error("worker: ack failed", "run_id", job.RunID, "error", err.Error())
	}
}

// heartbeatLoop pings /heartbeat on opts.HeartbeatEvery cadence until
// ctx is canceled. A 409 (lost ownership) terminates the loop early —
// the worker has been preempted by the reaper.
func (w *httpWorker) heartbeatLoop(ctx context.Context, runID string) {
	t := time.NewTicker(w.opts.HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.heartbeat(runID); err != nil {
				slog.Warn("heartbeat failed; stopping heartbeat loop",
					"run_id", runID, "error", err.Error())
				return
			}
		}
	}
}

func (w *httpWorker) heartbeat(runID string) error {
	url := strings.TrimRight(w.opts.ProducerURL, "/") + "/v1/jobs/" + runID + "/heartbeat"
	body, _ := json.Marshal(map[string]string{"worker": w.opts.WorkerID})
	return w.post(url, body, http.StatusOK)
}

func (w *httpWorker) ack(runID string, success bool, errMsg string) error {
	url := strings.TrimRight(w.opts.ProducerURL, "/") + "/v1/jobs/" + runID + "/ack"
	body, _ := json.Marshal(map[string]any{
		"worker":  w.opts.WorkerID,
		"success": success,
		"error":   errMsg,
	})
	return w.post(url, body, http.StatusOK)
}

func (w *httpWorker) post(url string, body []byte, expectCode int) error {
	req, err := http.NewRequestWithContext(w.ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", w.authHeader())
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != expectCode {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// controlLoop maintains an SSE subscription to /v1/workers/{id}/control
// for cancel signals. Reconnects on transient errors with backoff so a
// brief producer restart doesn't permanently disable cancel.
//
// The loop respects w.ctx — when the worker is shutting down, the
// SSE connection closes and we exit. Per-message handling routes to
// the run's cancel func via w.cancelRun.
func (w *httpWorker) controlLoop() {
	url := strings.TrimRight(w.opts.ProducerURL, "/") +
		"/v1/workers/" + w.opts.WorkerID + "/control"

	host, _ := os.Hostname()
	backoff := time.Second

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		req, err := http.NewRequestWithContext(w.ctx, http.MethodGet, url, nil)
		if err != nil {
			// non-recoverable construction error; just exit
			return
		}
		req.Header.Set("Authorization", w.authHeader())
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("X-Cronicle-Host", host)

		// SSE connection has no overall timeout — long-lived by design.
		// We use a separate client to avoid the long-poll client's
		// 60s+30s budget bleeding into here.
		sseClient := &http.Client{}
		resp, err := sseClient.Do(req)
		if err != nil {
			if w.ctx.Err() != nil {
				return
			}
			slog.Warn("control channel: connect failed; backing off",
				"error", err.Error(), "backoff", backoff)
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			slog.Warn("control channel: non-200; backing off",
				"status", resp.StatusCode,
				"body", strings.TrimSpace(string(body)),
				"backoff", backoff)
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		backoff = time.Second // reset on successful connect
		slog.Info("control channel: subscribed", "worker_id", w.opts.WorkerID)
		w.consumeSSE(resp.Body)
		// Connection closed (server hangup, network issue). Reconnect.
	}
}

// consumeSSE parses the SSE stream until the body closes. Lines we
// care about: `event: control` followed by `data: {...}` whose JSON
// has type=cancel + run_id. Pings are noted at debug level only.
func (w *httpWorker) consumeSSE(body io.ReadCloser) {
	defer body.Close()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var event string
	for scanner.Scan() {
		select {
		case <-w.ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if event == "control" {
				var msg state.ControlMsg
				if err := json.Unmarshal([]byte(data), &msg); err != nil {
					slog.Warn("control channel: bad data", "data", data)
					continue
				}
				w.handleControl(msg)
			}
		case line == "":
			event = "" // SSE message boundary
		}
	}
}

// handleControl dispatches an inbound control message to the matching
// in-flight run. Unknown types are logged-and-dropped — forward-compat
// with future verbs (drain, pause).
func (w *httpWorker) handleControl(msg state.ControlMsg) {
	switch msg.Type {
	case "cancel":
		if w.cancelRun(msg.RunID) {
			slog.Info("control: canceling run", "run_id", msg.RunID)
		} else {
			// Cancel arrived for a run we don't hold (already finished
			// locally, or the producer signaled the wrong worker). Benign.
			slog.Debug("control: cancel for unknown run", "run_id", msg.RunID)
		}
	case "ping":
		// no-op
	default:
		slog.Debug("control: unknown msg type", "type", msg.Type)
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

// hashOnly stays around as a placeholder for a future: if we ever
// want to anonymize hostnames in worker IDs (compliance scenarios),
// hashing host with a process-stable salt is the move.
//
//nolint:unused
func hashOnly(s string) string {
	return strings.ToLower(s)
}

// silence the unused import for sync — the worker uses sync.Once /
// sync.Mutex paths only on the producer side. Keep the import here
// reserved for follow-up work without re-shuffling each PR.
var _ sync.Mutex
