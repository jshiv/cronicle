package cronicle

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		stateSrc: func() *state.Store { return store },
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

// TestIngest_ServiceUnavailable: when the projection isn't available,
// surface 503 honestly rather than silently dropping events.
func TestIngest_ServiceUnavailable(t *testing.T) {
	srv := &listenServer{
		token:    "secret",
		stateSrc: func() *state.Store { return nil },
	}
	rr := httptest.NewRecorder()
	srv.handleIngestEvents(rr, ingestRequest(`{"entry_type":"schedule_start","run_id":"R","schedule":"x"}`))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rr.Code)
	}
}
