package cronicle

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// TestRunEvents_LivePrettyStream: an in-flight slog record routed via
// LiveSink arrives at the SSE consumer as pretty bytes wrapped in
// standard event-stream framing. The test exercises the full handler
// chain: slog.Info → Tagger → LiveSink (with pretty encoder) → SSE
// frame writer → client scanner.
//
// We use a real httptest.NewServer because the response body must
// stream piecemeal — ResponseRecorder buffers and would mask race
// conditions in the live tail logic.
func TestRunEvents_LivePrettyStream(t *testing.T) {
	// Build a LiveSink that uses the same pretty encoder cronicle wires
	// at startup. The trivial-text encoder would also work, but the
	// pretty path is what the operator actually sees, so we test it.
	ls := state.NewLiveSink(newLiveEncoder(LiveFormatPretty))

	// In-memory store satisfies the listener's stateSrc check; we don't
	// actually query it for live-only events.
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()

	srv := &listenServer{
		token:       "secret",
		stateSrc:    func() *state.Store { return store },
		liveSinkSrc: func() *state.LiveSink { return ls },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/runs/", srv.handleRunRoute)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/runs/R1/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d", resp.StatusCode)
	}

	// Scan SSE frames inline so we observe them as they arrive (not
	// buffered until body close).
	got := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var cur []string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				frame := strings.Join(cur, "\n")
				cur = nil
				// Skip heartbeat comment frames.
				if strings.HasPrefix(frame, ": ping") {
					continue
				}
				got <- frame
				return
			}
			cur = append(cur, line)
		}
		got <- ""
	}()

	// Give the handler a beat to Subscribe before publishing.
	time.Sleep(50 * time.Millisecond)

	// Push a stdout_chunk record through LiveSink directly. The pretty
	// encoder (suppressChunks=false for the LiveSink path) renders the
	// chunk as raw bytes — exactly what the frontend's xterm.js pane
	// needs.
	r := slog.NewRecord(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC), slog.LevelInfo,
		"hello from live", 0)
	r.AddAttrs(
		slog.String("entry_type", "stdout_chunk"),
		slog.String("run_id", "R1"),
		slog.String("task", "say-hi"),
		slog.String("schedule", "tick"),
		slog.String("stream", "stdout"),
	)
	_ = ls.Handle(context.Background(), r)

	select {
	case frame := <-got:
		if frame == "" {
			t.Fatal("got empty frame")
		}
		if !strings.HasPrefix(frame, "event: cronicle") {
			t.Errorf("missing SSE event prefix; frame=%q", frame)
		}
		if !strings.Contains(frame, "data: hello from live") {
			t.Errorf("missing rendered stdout_chunk in frame; frame=%q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE frame")
	}
}

// TestRunEvents_NoLiveSinkReturnsServiceUnavailable: the endpoint
// surfaces 503 honestly when state (and thus LiveSink) is disabled,
// rather than hanging.
func TestRunEvents_NoLiveSinkReturnsServiceUnavailable(t *testing.T) {
	store, _ := state.Open(":memory:")
	defer store.Close()
	srv := &listenServer{
		token:       "secret",
		stateSrc:    func() *state.Store { return store },
		liveSinkSrc: func() *state.LiveSink { return nil },
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/R1/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rr.Code)
	}
}

// TestScheduleEvents_LiveStream: subscribing to /v1/schedules/{name}/events
// delivers frames from any run of that schedule. This is the chicken-
// and-egg solver: the test subscribes BEFORE any record is published,
// then publishes — and the frame still arrives because LiveSink keys
// on schedule, not on a run_id we'd have to know first.
func TestScheduleEvents_LiveStream(t *testing.T) {
	ls := state.NewLiveSink(newLiveEncoder(LiveFormatPretty))
	store, _ := state.Open(":memory:")
	defer store.Close()

	// findSchedule needs a Config — provide one that has the schedule
	// the test is subscribing to. The dispatcher 404s if missing.
	conf := &Config{
		Schedules: []Schedule{{Name: "story"}},
	}
	srv := &listenServer{
		token:       "secret",
		confSrc:     func() *Config { return conf },
		stateSrc:    func() *state.Store { return store },
		liveSinkSrc: func() *state.LiveSink { return ls },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/schedules/", srv.handleScheduleRoute)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/schedules/story/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d", resp.StatusCode)
	}

	got := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var cur []string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				frame := strings.Join(cur, "\n")
				cur = nil
				if strings.HasPrefix(frame, ": ping") {
					continue
				}
				got <- frame
				return
			}
			cur = append(cur, line)
		}
		got <- ""
	}()

	// Let the subscriber register, then publish with a brand-new
	// run_id the SSE caller never knew. The schedule filter still
	// catches the frame.
	time.Sleep(50 * time.Millisecond)
	r := slog.NewRecord(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
		slog.LevelInfo, "live from new run", 0)
	r.AddAttrs(
		slog.String("entry_type", "stdout_chunk"),
		slog.String("run_id", "20260511T999999Z-justnow"),
		slog.String("schedule", "story"),
		slog.String("task", "tell-a-story"),
		slog.String("stream", "stdout"),
	)
	_ = ls.Handle(context.Background(), r)

	select {
	case frame := <-got:
		if !strings.Contains(frame, "data: live from new run") {
			t.Fatalf("schedule subscriber missed the frame: %q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for schedule SSE frame")
	}
}

// TestScheduleEvents_UnknownSchedule404: misspelled or unloaded
// schedules return 404 honestly rather than hanging the client on a
// never-firing subscription.
func TestScheduleEvents_UnknownSchedule404(t *testing.T) {
	conf := &Config{Schedules: []Schedule{{Name: "real"}}}
	srv := &listenServer{
		token:       "secret",
		confSrc:     func() *Config { return conf },
		liveSinkSrc: func() *state.LiveSink { return state.NewLiveSink(newLiveEncoder(LiveFormatPretty)) },
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/schedules/typo/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rr.Code)
	}
}

// TestEventsStream_Firehose: GET /v1/events/stream delivers records
// from ANY schedule. The single subscriber sees frames from "story"
// and "tick" both, proving Filter{} matches everything.
func TestEventsStream_Firehose(t *testing.T) {
	ls := state.NewLiveSink(newLiveEncoder(LiveFormatPretty))
	srv := &listenServer{
		token:       "secret",
		liveSinkSrc: func() *state.LiveSink { return ls },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/events/stream", srv.handleEventsStream)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/events/stream", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("got %d", resp.StatusCode)
	}

	frames := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var cur []string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				frame := strings.Join(cur, "\n")
				cur = nil
				if strings.HasPrefix(frame, ": ping") {
					continue
				}
				select {
				case frames <- frame:
				default:
					return
				}
				continue
			}
			cur = append(cur, line)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	for _, sched := range []string{"story", "tick"} {
		r := slog.NewRecord(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
			slog.LevelInfo, sched+" line", 0)
		r.AddAttrs(
			slog.String("entry_type", "stdout_chunk"),
			slog.String("run_id", "R-"+sched),
			slog.String("schedule", sched),
			slog.String("task", "t1"),
			slog.String("stream", "stdout"),
		)
		_ = ls.Handle(context.Background(), r)
	}

	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case f := <-frames:
			if strings.Contains(f, "data: story line") {
				seen["story"] = true
			}
			if strings.Contains(f, "data: tick line") {
				seen["tick"] = true
			}
		case <-deadline:
			t.Fatalf("firehose got %v, want both story and tick", seen)
		}
	}
}

// TestRunEvents_AuthRequired: bearer token check applies.
func TestRunEvents_AuthRequired(t *testing.T) {
	srv := &listenServer{token: "secret"}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/R1/events", nil)
	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rr.Code)
	}
}

// TestWriteSSEFrame_MultilineSplit: a pretty-rendered multi-line
// payload becomes one event with one `data:` line per source line, per
// SSE spec. Browsers join with '\n' on receive.
func TestWriteSSEFrame_MultilineSplit(t *testing.T) {
	var buf strings.Builder
	writeSSEFrame(&buf, []byte("line one\nline two\nline three"))
	got := buf.String()
	want := "event: cronicle\ndata: line one\ndata: line two\ndata: line three\n\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestWriteSSEFrame_TrailingNewline: a payload ending in '\n' shouldn't
// produce a trailing empty `data:` line. SSE spec is forgiving but a
// stray empty data is ugly on the wire.
func TestWriteSSEFrame_TrailingNewline(t *testing.T) {
	var buf strings.Builder
	writeSSEFrame(&buf, []byte("only line\n"))
	got := buf.String()
	want := "event: cronicle\ndata: only line\n\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestWriteSSEFrame_EmptyPayloadDropped: nothing in → nothing out.
// Avoids stuttering empty events when an encoder filters a record.
func TestWriteSSEFrame_EmptyPayloadDropped(t *testing.T) {
	var buf strings.Builder
	writeSSEFrame(&buf, nil)
	writeSSEFrame(&buf, []byte{})
	if buf.Len() != 0 {
		t.Fatalf("expected no output, got %q", buf.String())
	}
}

// TestLineEmitter_OneRecordPerNewline confirms the lineEmitter splits
// at \n and emits one slog record per completed line. Buffer state
// across calls is exercised by writing a partial line then completing
// it.
func TestLineEmitter_OneRecordPerNewline(t *testing.T) {
	collected := &slogCollector{}
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(collected))

	le := newLineEmitter(&noopWriter{}, "R1", "say-hi", "tick", "stdout")
	le.Write([]byte("hello world\n"))
	le.Write([]byte("partial "))
	le.Write([]byte("continued\nfinal"))
	le.Flush()

	lines := collected.lines("stdout_chunk")
	want := []string{"hello world", "partial continued", "final"}
	if len(lines) != len(want) {
		t.Fatalf("got %d records, want %d: %v", len(lines), len(want), lines)
	}
	for i, line := range want {
		if lines[i] != line {
			t.Errorf("record %d: got %q, want %q", i, lines[i], line)
		}
	}
}

// TestLineEmitter_StderrTagsEntryType: stderr stream produces
// entry_type=stderr_chunk so consumers can route/color separately.
func TestLineEmitter_StderrTagsEntryType(t *testing.T) {
	collected := &slogCollector{}
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(collected))

	le := newLineEmitter(&noopWriter{}, "R1", "t", "s", "stderr")
	le.Write([]byte("oops\n"))

	if got := collected.lines("stderr_chunk"); len(got) != 1 || got[0] != "oops" {
		t.Fatalf("stderr_chunk records: %v", got)
	}
	if got := collected.lines("stdout_chunk"); len(got) != 0 {
		t.Fatalf("did not expect stdout_chunk records: %v", got)
	}
}

// slogCollector captures every slog.Record so tests can assert on
// emitted msg + entry_type. Concurrency-safe; the line emitter
// uses its own mutex but a test may run multiple goroutines.
type slogCollector struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *slogCollector) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (c *slogCollector) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.records = append(c.records, r)
	c.mu.Unlock()
	return nil
}
func (c *slogCollector) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *slogCollector) WithGroup(_ string) slog.Handler      { return c }

// lines returns the msg field of every collected record with the given
// entry_type, in arrival order.
func (c *slogCollector) lines(entryType string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.records {
		var et string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "entry_type" {
				et = a.Value.String()
				return false
			}
			return true
		})
		if et == entryType {
			out = append(out, r.Message)
		}
	}
	return out
}

// noopWriter swallows bytes — the lineEmitter forwards to its inner
// writer first; for tests we don't need the bytes to land anywhere.
type noopWriter struct{}

func (*noopWriter) Write(p []byte) (int, error) { return len(p), nil }
