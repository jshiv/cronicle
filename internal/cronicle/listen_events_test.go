package cronicle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// eventsHarness wires a real in-memory state store to a listenServer
// so ingest tests exercise the full path: HTTP → DecodeEvent → Apply →
// projection.
func eventsHarness(t *testing.T) (*listenServer, *state.Store) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &listenServer{
		token:    "secret",
		stateSrc: func() state.Backend { return store },
	}, store
}

func ingestRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

// TestIngest_AuthRequired: bearer token check applies.
func TestIngest_AuthRequired(t *testing.T) {
	srv, _ := eventsHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(""))
	srv.handleIngestEvents(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rr.Code)
	}
}

// TestIngest_MethodNotAllowed: only POST is accepted.
func TestIngest_MethodNotAllowed(t *testing.T) {
	srv, _ := eventsHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	srv.handleIngestEvents(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", rr.Code)
	}
}

// TestIngest_HappyPath: a JSONL batch lands in the projection. Reuses
// the same shape that cronicle.jsonl already produces — the wire and
// the file are identical, by intent.
func TestIngest_HappyPath(t *testing.T) {
	srv, store := eventsHarness(t)
	body := strings.Join([]string{
		`{"time":"2026-05-10T12:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"daily","tasks":["a"]}`,
		`{"time":"2026-05-10T12:00:01Z","entry_type":"task_start","run_id":"R1","schedule":"daily","task":"a","attempt":1}`,
		`{"time":"2026-05-10T12:00:02Z","entry_type":"shell_run","run_id":"R1","schedule":"daily","task":"a","exit":0,"duration_ms":50,"success":true}`,
		`{"time":"2026-05-10T12:00:03Z","entry_type":"schedule_complete","run_id":"R1","schedule":"daily","task_count":1,"duration_ms":80,"success":true}`,
	}, "\n")

	rr := httptest.NewRecorder()
	srv.handleIngestEvents(rr, ingestRequest(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp ingestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 4 || resp.Dropped != 0 {
		t.Fatalf("counts: %+v", resp)
	}
	r, err := store.GetRun("R1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.Status != "succeeded" || len(r.Tasks) != 1 || r.Tasks[0].Status != "succeeded" {
		t.Fatalf("projection state wrong: %+v", r)
	}
}

// TestIngest_PartialBadLines: malformed lines are counted dropped, the
// rest still apply. Empty lines at EOF or between entries are ignored.
func TestIngest_PartialBadLines(t *testing.T) {
	srv, store := eventsHarness(t)
	body := strings.Join([]string{
		`{"time":"2026-05-10T12:00:00Z","entry_type":"schedule_start","run_id":"R2","schedule":"x","tasks":["only"]}`,
		``, // empty line — ignored
		`{not valid json`,                                         // dropped (parse fail)
		`{"msg":"no entry_type but valid json","level":"INFO"}`,   // dropped (no entry_type)
		`{"entry_type":"task_start","schedule":"x","task":"only"}`, // dropped (no run_id)
		`{"time":"2026-05-10T12:00:01Z","entry_type":"task_start","run_id":"R2","schedule":"x","task":"only","attempt":1}`,
	}, "\n")
	rr := httptest.NewRecorder()
	srv.handleIngestEvents(rr, ingestRequest(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	var resp ingestResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Accepted != 2 {
		t.Fatalf("accepted: got %d, want 2", resp.Accepted)
	}
	if resp.Dropped != 3 {
		t.Fatalf("dropped: got %d, want 3", resp.Dropped)
	}
	if n, _ := store.CountRuns(); n != 1 {
		t.Fatalf("runs created: got %d, want 1", n)
	}
}

// TestIngest_EmptyBody: zero events is a 200 with accepted=0; no error.
func TestIngest_EmptyBody(t *testing.T) {
	srv, _ := eventsHarness(t)
	rr := httptest.NewRecorder()
	srv.handleIngestEvents(rr, ingestRequest(""))
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}
	var resp ingestResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Accepted != 0 || resp.Dropped != 0 {
		t.Fatalf("expected zeros, got %+v", resp)
	}
}

// TestIngest_BodyTooLarge: a body over 16 MiB should be rejected with
// 413 — protects the producer from a misbehaving worker. We don't ship
// a 16 MiB payload here; instead we exercise the limit by faking the
// request body size with a wrapped reader.
func TestIngest_BodyTooLarge(t *testing.T) {
	srv, _ := eventsHarness(t)
	// One line, but we crank up MaxBytesReader's bound by sending a body
	// that's too big to fit. Simpler approach: synthesize 17 MiB of 'a's
	// — they all parse-fail individually but the scanner trips the limit
	// first because each line is 17 MiB long and exceeds the buffer.
	huge := bytes.Repeat([]byte("a"), 17<<20)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(huge))
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleIngestEvents(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413; body=%s", rr.Code, rr.Body.String())
	}
}

// TestIngest_FanoutsToLiveSink: distributed-mode workers POST their
// slog records to /v1/events. The runner must republish them to the
// in-memory LiveSink so SSE subscribers (which only see the runner's
// own slog chain via Handle) get visibility into worker-executed work.
//
// Without this fan-out, /v1/runs/{id}/events on the runner would show
// runner-locally-executed records only — worker records would persist
// to SQLite fine but never reach the live stream. That's the visibility
// gap a frontend operator hits when they trigger a job that lands on a
// worker node and see an empty SSE pane.
//
// This test wires the real ingest endpoint to a real LiveSink, opens
// a real SSE subscription over httptest, POSTs a JSONL line, and
// verifies the subscriber receives the event bytes verbatim.
func TestIngest_FanoutsToLiveSink(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()

	ls := state.NewLiveSink(newLiveEncoder(LiveFormatPretty))
	srv := &listenServer{
		token:       "secret",
		stateSrc:    func() state.Backend { return store },
		liveSinkSrc: func() *state.LiveSink { return ls },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/events", srv.handleIngestEvents)
	mux.HandleFunc("/v1/runs/", srv.handleRunRoute)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Subscriber side: open an SSE GET on the run_id the worker will
	// publish under. The subscription must be alive before the POST or
	// the LiveSink fan-out has no consumer to deliver to (LiveSink is
	// pub/sub with no replay buffer — the same shape the production code
	// uses).
	subReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/runs/WORKER-R1/events", nil)
	subReq.Header.Set("Authorization", "Bearer secret")
	subResp, err := http.DefaultClient.Do(subReq)
	if err != nil {
		t.Fatalf("subscribe GET: %v", err)
	}
	defer subResp.Body.Close()
	if subResp.StatusCode != 200 {
		t.Fatalf("subscribe: got %d", subResp.StatusCode)
	}

	frames := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(subResp.Body)
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
				frames <- frame
				return
			}
			cur = append(cur, line)
		}
		frames <- ""
	}()

	// Give the SSE handler a beat to Subscribe before the POST fires.
	time.Sleep(50 * time.Millisecond)

	// Worker-shipper side: POST one JSONL event. Shape matches what
	// worker_event_ship.go writes — generic JSON with time/level/msg
	// plus the entry_type/run_id/schedule/task routing attrs.
	body := `{"time":"2026-05-11T12:00:00Z","level":"INFO","msg":"hello from worker","entry_type":"stdout_chunk","run_id":"WORKER-R1","schedule":"distributed","task":"remote-task","stream":"stdout"}`
	rr := httptest.NewRecorder()
	srv.handleIngestEvents(rr, ingestRequest(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("ingest: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp ingestResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Accepted != 1 {
		t.Fatalf("accepted: got %d, want 1", resp.Accepted)
	}

	select {
	case frame := <-frames:
		if frame == "" {
			t.Fatal("empty SSE frame — subscriber received nothing")
		}
		// After rehydrate + re-encode, the subscriber sees the runner's
		// pretty form, NOT the worker's raw JSON. renderStdoutChunk
		// renders only the message line; the run_id stays in the
		// routing tags (used by fan-out filter) but doesn't appear in
		// the encoded payload.
		if !strings.Contains(frame, "data: hello from worker") {
			t.Fatalf("subscriber missed pretty-rendered chunk; frame=%q", frame)
		}
		// JSON wire-shape leakage check: if the run_id key appears in
		// the frame, we're back to raw-JSON pass-through and the format
		// unification regressed.
		if strings.Contains(frame, `"run_id"`) {
			t.Fatalf("frame leaked raw JSON shape; want pretty encoding only; frame=%q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out — worker event did not reach SSE subscriber")
	}
}

// TestIngest_AgentDeltaRendersPretty: workers run agents in distributed
// mode (the heavy work). Text deltas (token-by-token model output) MUST
// reach the SSE pane in the same ANSI-pretty form a runner-local agent
// would produce — anything else means a frontend operator gets raw JSON
// when watching a worker-executed agent, while a runner-executed agent
// shows clean text. The whole point of unifying the wire format on the
// runner side is that the operator can't tell the difference.
//
// Mechanically: rehydrate must reconstruct a slog.Record that
// renderTextDelta accepts. The renderer reads r.Message directly and
// writes it without a trailing newline (the model controls its own
// formatting), so the SSE frame's `data:` line carries the token
// exactly as the model produced it.
func TestIngest_AgentDeltaRendersPretty(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()

	ls := state.NewLiveSink(newLiveEncoder(LiveFormatPretty))
	srv := &listenServer{
		token:       "secret",
		stateSrc:    func() state.Backend { return store },
		liveSinkSrc: func() *state.LiveSink { return ls },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/events", srv.handleIngestEvents)
	mux.HandleFunc("/v1/runs/", srv.handleRunRoute)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	subReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/runs/AGENT-RUN/events", nil)
	subReq.Header.Set("Authorization", "Bearer secret")
	subResp, err := http.DefaultClient.Do(subReq)
	if err != nil {
		t.Fatalf("subscribe GET: %v", err)
	}
	defer subResp.Body.Close()

	frames := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(subResp.Body)
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
				frames <- frame
				return
			}
			cur = append(cur, line)
		}
		frames <- ""
	}()

	time.Sleep(50 * time.Millisecond)

	// Worker ships a single agent text_delta carrying the model's token.
	// Numeric attrs (output_tokens) go through json.Number so the
	// rehydrate produces Int64Kind — same as a runner-local record.
	body := `{"time":"2026-05-11T12:00:00Z","level":"INFO","msg":"The quick brown fox","entry_type":"text_delta","run_id":"AGENT-RUN","schedule":"agent-demo","task":"summarize","output_tokens":4}`
	rr := httptest.NewRecorder()
	srv.handleIngestEvents(rr, ingestRequest(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("ingest: got %d", rr.Code)
	}

	select {
	case frame := <-frames:
		// renderTextDelta writes just r.Message — no decoration, no
		// newline added. SSE frame is then `event: cronicle` / `data: The quick brown fox`.
		if !strings.Contains(frame, "data: The quick brown fox") {
			t.Fatalf("agent delta did not render as pretty text; frame=%q", frame)
		}
		if strings.Contains(frame, `"entry_type"`) {
			t.Fatalf("frame leaked raw JSON shape from worker; frame=%q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out — agent delta did not reach SSE subscriber")
	}
}

// TestIngest_ServiceUnavailable: when the projection isn't available,
// surface 503 honestly rather than silently dropping events.
func TestIngest_ServiceUnavailable(t *testing.T) {
	srv := &listenServer{
		token:    "secret",
		stateSrc: func() state.Backend { return nil },
	}
	rr := httptest.NewRecorder()
	srv.handleIngestEvents(rr, ingestRequest(`{"entry_type":"schedule_start","run_id":"R","schedule":"x"}`))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rr.Code)
	}
}
