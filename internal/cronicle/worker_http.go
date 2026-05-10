package cronicle

import (
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

// execute runs the job and acks it. Heartbeats while running. The
// schedule's events stay local to the worker via slog (file + stdout
// + memory projection); a future enhancement is to POST them back to
// the producer via /v1/events.
func (w *httpWorker) execute(croniclePath string, job state.Job) {
	slog.Info("worker claimed job",
		"run_id", job.RunID,
		"schedule", job.Schedule,
		"attempt", job.Attempt,
	)

	hbCtx, hbCancel := context.WithCancel(w.ctx)
	defer hbCancel()
	go w.heartbeatLoop(hbCtx, job.RunID)

	var sch Schedule
	success := true
	errMsg := ""
	if err := json.Unmarshal(job.Payload, &sch); err != nil {
		slog.Error("worker: bad payload", "run_id", job.RunID, "error", err.Error())
		success = false
		errMsg = err.Error()
	} else {
		// Stamp source so the projection's runs row records "executed
		// on a remote worker" rather than reusing whatever value the
		// producer set. Most workers will run cron-fired or HTTP-fired
		// schedules; we keep the producer's source if already set.
		if sch.Source == "" {
			sch.Source = "worker"
		}
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
	}

	hbCancel()
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
