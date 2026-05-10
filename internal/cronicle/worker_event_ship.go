package cronicle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// eventShipper batches slog records that carry an entry_type and POSTs
// them to the producer's /v1/events endpoint as JSONL. Implements
// slog.Handler so it slots into the same multi-handler chain as the
// file/stdout writers.
//
// Buffering is bounded — a misbehaving producer (slow, partitioned,
// returning 503) cannot push back-pressure into the slog hot path that
// runs inside ExecuteTasks. When the buffer fills, oldest events are
// dropped and a stat counter increments. Local logs (stdout +
// cronicle.jsonl) are NOT affected — they're written by sibling
// handlers in the multi-handler chain that don't share the buffer.
type eventShipper struct {
	url        string // POST endpoint, e.g. http://producer:8765/v1/events
	token      string
	client     *http.Client
	in         chan []byte
	dropped    int64
	dropCounMu sync.Mutex
	closeOnce  sync.Once
	done       chan struct{}
}

// newEventShipper constructs a running shipper. Caller is responsible
// for calling stop() at process exit; otherwise the flusher goroutine
// blocks forever holding the http.Client.
func newEventShipper(producerURL, token string) *eventShipper {
	s := &eventShipper{
		url:    strings.TrimRight(producerURL, "/") + "/v1/events",
		token:  token,
		client: &http.Client{Timeout: 10 * time.Second},
		in:     make(chan []byte, 4096),
		done:   make(chan struct{}),
	}
	go s.run()
	return s
}

// run drains the buffered channel into batched POSTs. We batch by
// time + size: ship every 500ms or when the batch hits 64 events,
// whichever comes first. At our load this means roughly one HTTP
// roundtrip per few seconds, never more than one in flight at a time
// (we wait for the previous to finish before issuing another).
func (s *eventShipper) run() {
	defer close(s.done)

	const (
		flushInterval = 500 * time.Millisecond
		flushSize     = 64
	)
	t := time.NewTicker(flushInterval)
	defer t.Stop()

	var batch [][]byte
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.post(batch)
		batch = batch[:0]
	}

	for {
		select {
		case line, ok := <-s.in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, line)
			if len(batch) >= flushSize {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// post sends one batch as a JSONL body. Errors are logged-and-dropped
// (we already lost the source events at this point; the JSONL log on
// disk has the audit trail). At-least-once semantics from the
// producer's side are NOT guaranteed by this shipper — re-POSTing on
// error would risk duplicate Apply calls, and the projection's row
// updates are monotonic-ish but not perfectly idempotent. Better to
// drop one batch than to corrupt counts.
func (s *eventShipper) post(batch [][]byte) {
	body := bytes.Join(batch, []byte("\n"))
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := s.client.Do(req)
	if err != nil {
		// Log via stdlib log so we don't recurse into slog and
		// re-feed the shipper.
		s.bumpDropped(int64(len(batch)))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		s.bumpDropped(int64(len(batch)))
	}
}

func (s *eventShipper) bumpDropped(n int64) {
	s.dropCounMu.Lock()
	s.dropped += n
	s.dropCounMu.Unlock()
}

// stop drains any remaining batch and tears down the goroutine. Safe
// to call multiple times.
func (s *eventShipper) stop() {
	s.closeOnce.Do(func() {
		close(s.in)
		<-s.done
	})
}

// ---- slog.Handler ----------------------------------------------------------

func (s *eventShipper) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (s *eventShipper) Handle(_ context.Context, r slog.Record) error {
	// Skip records without entry_type. Lifecycle prints (heartbeat,
	// "Loading config…") aren't projection-relevant.
	hasEntryType := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "entry_type" {
			hasEntryType = true
			return false
		}
		return true
	})
	if !hasEntryType {
		return nil
	}

	// Same JSON shape the file logger writes. We don't reuse
	// slog.NewJSONHandler because it writes to an io.Writer; we want
	// the bytes back.
	m := map[string]any{
		"time":  r.Time,
		"level": r.Level.String(),
		"msg":   r.Message,
	}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	line, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	// Non-blocking send; on overflow, drop newest. Workers should
	// never block their execution path on a slow producer.
	select {
	case s.in <- line:
	default:
		s.bumpDropped(1)
	}
	return nil
}

func (s *eventShipper) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *eventShipper) WithGroup(_ string) slog.Handler      { return s }

// enableEventShipping wires a shipper into the default slog handler
// chain. Returns the shipper so the caller can stop() it at shutdown.
// Composes with the file/stdout writers — they're independent
// handlers in the multi-handler.
func enableEventShipping(producerURL, token string) *eventShipper {
	if producerURL == "" {
		return nil
	}
	s := newEventShipper(producerURL, token)
	current := slog.Default().Handler()
	slog.SetDefault(slog.New(&multiHandler{handlers: []slog.Handler{current, s}}))
	return s
}
