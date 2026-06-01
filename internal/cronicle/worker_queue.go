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
	"strings"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// queueClient is how a worker pulls jobs from the producer's queue and
// settles them. There are two implementations and ONE worker loop:
//
//   - httpQueueClient — remote workers, over the producer's /v1/jobs HTTP
//     API (the distributed deployment).
//   - localQueueClient — the in-process self-worker, straight over the
//     state backend. No socket: single-node talks to its own queue with a
//     function call, not a loopback request.
//
// The self-worker holding a localQueueClient (rather than a raw
// state.Backend) is the point: its only contact with state is these three
// queue verbs — it cannot read run history or projections, so the producer
// stays the single source of truth without relying on convention.
type queueClient interface {
	// Claim returns the next job, blocking up to block for one to appear.
	// (job, false, nil) means "nothing within block" — the caller loops.
	Claim(block time.Duration) (state.Job, bool, error)
	Ack(runID string, success bool, errMsg string) error
	Heartbeat(runID string) error
}

// localVisibility is how long a claimed job is invisible to other workers
// before the reaper may reclaim it. Matches the producer-side default.
const localVisibility = 5 * time.Minute

// localQueueClient drives the queue in-process for the single-node
// self-worker. It is the ONLY place the worker side touches the store, and
// only through Claim/Ack/Heartbeat.
type localQueueClient struct {
	store    state.Backend
	workerID string
	ctx      context.Context
}

func (c *localQueueClient) Claim(block time.Duration) (state.Job, bool, error) {
	job, err := c.store.Claim(c.workerID, localVisibility)
	if errors.Is(err, state.ErrNoJobs) {
		// Park until a wakeup or block elapses, then report "no job" so the
		// worker loops and re-claims. ctx lets shutdown interrupt the park.
		c.store.WaitForJob(c.ctx, block)
		return state.Job{}, false, nil
	}
	if err != nil {
		return state.Job{}, false, err
	}
	return job, true, nil
}

func (c *localQueueClient) Ack(runID string, success bool, errMsg string) error {
	return c.store.Ack(runID, c.workerID, success, errMsg)
}

func (c *localQueueClient) Heartbeat(runID string) error {
	return c.store.Heartbeat(runID, c.workerID, localVisibility)
}

// httpQueueClient drives the queue over the producer's HTTP API. It also
// owns the SSE control channel (cancel signals), which is inherently a
// remote concern — the local worker has no equivalent.
type httpQueueClient struct {
	producerURL string
	token       string
	workerID    string
	client      *http.Client
	ctx         context.Context
}

func (c *httpQueueClient) authHeader() string { return "Bearer " + c.token }

func (c *httpQueueClient) Claim(block time.Duration) (state.Job, bool, error) {
	url := strings.TrimRight(c.producerURL, "/") +
		"/v1/jobs?worker=" + c.workerID +
		"&block=" + block.String()
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, url, nil)
	if err != nil {
		return state.Job{}, false, err
	}
	req.Header.Set("Authorization", c.authHeader())
	resp, err := c.client.Do(req)
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

func (c *httpQueueClient) Heartbeat(runID string) error {
	url := strings.TrimRight(c.producerURL, "/") + "/v1/jobs/" + runID + "/heartbeat"
	body, _ := json.Marshal(map[string]string{"worker": c.workerID})
	return c.post(url, body, http.StatusOK)
}

func (c *httpQueueClient) Ack(runID string, success bool, errMsg string) error {
	url := strings.TrimRight(c.producerURL, "/") + "/v1/jobs/" + runID + "/ack"
	body, _ := json.Marshal(map[string]any{
		"worker":  c.workerID,
		"success": success,
		"error":   errMsg,
	})
	return c.post(url, body, http.StatusOK)
}

func (c *httpQueueClient) post(url string, body []byte, expectCode int) error {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
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
// for cancel signals, dispatching each to cancel(runID). Reconnects on
// transient errors with backoff so a brief producer restart doesn't
// permanently disable cancel. Respects ctx for shutdown. Remote-only —
// the in-process self-worker has no control channel.
func (c *httpQueueClient) controlLoop(cancel func(runID string) bool) {
	url := strings.TrimRight(c.producerURL, "/") +
		"/v1/workers/" + c.workerID + "/control"

	host, _ := os.Hostname()
	backoff := time.Second

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", c.authHeader())
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("X-Cronicle-Host", host)

		// SSE connection has no overall timeout — long-lived by design.
		sseClient := &http.Client{}
		resp, err := sseClient.Do(req)
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			slog.Warn("control channel: connect failed; backing off",
				"error", err.Error(), "backoff", backoff)
			select {
			case <-c.ctx.Done():
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
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		backoff = time.Second // reset on successful connect
		slog.Info("control channel: subscribed", "worker_id", c.workerID)
		c.consumeSSE(resp.Body, cancel)
		// Connection closed (server hangup, network issue). Reconnect.
	}
}

// consumeSSE parses the SSE stream until the body closes, routing each
// control message to cancel(runID).
func (c *httpQueueClient) consumeSSE(body io.ReadCloser, cancel func(runID string) bool) {
	defer body.Close()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var event string
	for scanner.Scan() {
		select {
		case <-c.ctx.Done():
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
				handleControl(msg, cancel)
			}
		case line == "":
			event = "" // SSE message boundary
		}
	}
}

// handleControl dispatches an inbound control message via cancel. Unknown
// types are logged-and-dropped — forward-compat with future verbs.
func handleControl(msg state.ControlMsg, cancel func(runID string) bool) {
	switch msg.Type {
	case "cancel":
		if cancel(msg.RunID) {
			slog.Info("control: canceling run", "run_id", msg.RunID)
		} else {
			slog.Debug("control: cancel for unknown run", "run_id", msg.RunID)
		}
	case "ping":
		// no-op
	default:
		slog.Debug("control: unknown msg type", "type", msg.Type)
	}
}
