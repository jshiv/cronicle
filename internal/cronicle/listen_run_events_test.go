package cronicle

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// TestParseEventID covers the new Last-Event-ID shape "<lifetime>-<seq>"
// plus the legacy bare-int and empty cases. We hand-pick edges that the
// hot path (parseEventID called once per SSE GET) is likely to see in
// the wild: trailing/leading dash, multi-dash lifetimes (forward-compat
// reserve), non-numeric seq.
func TestParseEventID(t *testing.T) {
	cases := []struct {
		in        string
		wantLT    string
		wantSeq   int64
		wantEmpty bool
	}{
		{in: "", wantEmpty: true},
		{in: "abc12345-42", wantLT: "abc12345", wantSeq: 42},
		// Empty seq segment ⇒ unparseable.
		{in: "abc12345-", wantEmpty: true},
		// Empty lifetime ⇒ unparseable (avoid colliding with legacy bare int).
		{in: "-42", wantEmpty: true},
		// Non-numeric seq ⇒ unparseable.
		{in: "abc-xyz", wantEmpty: true},
		// Multi-dash: we split on LAST dash so "uuid-with-dashes-42" works.
		{in: "uuid-with-dashes-42", wantLT: "uuid-with-dashes", wantSeq: 42},
	}
	for _, c := range cases {
		gotLT, gotSeq := parseEventID(c.in)
		if c.wantEmpty {
			if gotLT != "" || gotSeq != 0 {
				t.Errorf("parseEventID(%q): want empty, got (%q, %d)", c.in, gotLT, gotSeq)
			}
			continue
		}
		if gotLT != c.wantLT || gotSeq != c.wantSeq {
			t.Errorf("parseEventID(%q): got (%q, %d), want (%q, %d)",
				c.in, gotLT, gotSeq, c.wantLT, c.wantSeq)
		}
	}
}

// TestExtractIDFromPayload: live-tail records carry seq+lifetime in
// their JSON body (Tagger injected them upstream). The handler peels
// them back out via a partial unmarshal to construct the SSE id.
func TestExtractIDFromPayload(t *testing.T) {
	line := []byte(`{"time":"2026-05-10T12:00:00Z","msg":"x","entry_type":"task_start","run_id":"R","seq":7,"lifetime":"deadbeef"}`)
	lt, seq := extractIDFromPayload(line)
	if lt != "deadbeef" || seq != 7 {
		t.Fatalf("got (%q, %d), want (deadbeef, 7)", lt, seq)
	}

	// Pre-Tagger payload (no seq/lifetime): both zero values, no panic.
	lt, seq = extractIDFromPayload([]byte(`{"entry_type":"task_start","run_id":"R"}`))
	if lt != "" || seq != 0 {
		t.Fatalf("untagged: got (%q, %d), want (\"\", 0)", lt, seq)
	}

	// Malformed JSON ⇒ zero values, the caller emits an idless frame.
	lt, seq = extractIDFromPayload([]byte("garbage"))
	if lt != "" || seq != 0 {
		t.Fatalf("garbage: got (%q, %d), want (\"\", 0)", lt, seq)
	}
}

// runEventsHarness wires a real in-memory store + a fresh LiveSink to a
// listenServer for end-to-end SSE testing. Returns the server and a
// cleanup so the caller can run requests against handleRunRoute.
func runEventsHarness(t *testing.T) (*listenServer, *state.Store, *state.LiveSink) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ls := state.NewLiveSink()
	srv := &listenServer{
		token:       "secret",
		stateSrc:    func() *state.Store { return store },
		liveSinkSrc: func() *state.LiveSink { return ls },
	}
	return srv, store, ls
}

// seedRunRow folds a fresh schedule_start event into store for runID
// with the given seq/lifetime, so tests have a row that EventsResume
// will return. Uses the LiveSink-style encoded line shape so the
// payload field round-trips correctly. The slog-chain Tagger isn't
// involved here — we set Seq/Lifetime directly via the typed Event.
func seedEvent(t *testing.T, store *state.Store, runID, entryType, lifetime string, seq int64) {
	t.Helper()
	ev := state.Event{
		Time:      time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
		EntryType: entryType,
		RunID:     runID,
		Schedule:  "test",
		Seq:       seq,
		Lifetime:  lifetime,
	}
	if entryType == "schedule_start" {
		ev.Tasks = []string{"a"}
	}
	if err := store.Apply(ev); err != nil {
		t.Fatalf("Apply %s/%d: %v", lifetime, seq, err)
	}
}

// TestRunEvents_SSEIDFormat: a replay-only SSE response uses the
// "<lifetime>-<seq>" id form for every frame. This is the wire-format
// contract clients dedup against.
func TestRunEvents_SSEIDFormat(t *testing.T) {
	srv, store, _ := runEventsHarness(t)

	// Two tagged events, one per known SSE frame.
	seedEvent(t, store, "R1", "schedule_start", "lifeA", 1)
	seedEvent(t, store, "R1", "task_start", "lifeA", 2)

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/R1/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	// Run the handler with a context the test controls so it returns
	// after the replay block (no live source ⇒ handler exits when the
	// replay loop is done).
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "id: lifeA-1\n") {
		t.Errorf("missing id frame for seq=1; body=%s", body)
	}
	if !strings.Contains(body, "id: lifeA-2\n") {
		t.Errorf("missing id frame for seq=2; body=%s", body)
	}
	// The legacy bare-int id form must NOT appear when both seq and
	// lifetime are present — that's the wire-format regression we
	// guard against here.
	if strings.Contains(body, "id: 1\n") {
		t.Errorf("legacy bare-int id leaked into response; body=%s", body)
	}
}

// TestRunEvents_LastEventIDResume: a client reconnecting with
// Last-Event-ID = "lifeA-1" only receives events with seq > 1 (in the
// same lifetime). Older replay rows are suppressed.
func TestRunEvents_LastEventIDResume(t *testing.T) {
	srv, store, _ := runEventsHarness(t)
	seedEvent(t, store, "R1", "schedule_start", "lifeA", 1)
	seedEvent(t, store, "R1", "task_start", "lifeA", 2)
	seedEvent(t, store, "R1", "task_start", "lifeA", 3)

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/R1/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Last-Event-ID", "lifeA-2")
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, req)
	body := rr.Body.String()
	if strings.Contains(body, "id: lifeA-1\n") {
		t.Errorf("client saw lifeA-1 even though it resumed past it: %s", body)
	}
	if strings.Contains(body, "id: lifeA-2\n") {
		t.Errorf("client saw lifeA-2 even though it resumed past it: %s", body)
	}
	if !strings.Contains(body, "id: lifeA-3\n") {
		t.Errorf("client missed lifeA-3 (the only event past resume cursor): %s", body)
	}
}

// TestRunEvents_LifetimeMismatchReplaysAll: producer restarted between
// the client's prior connection and now. Client's Last-Event-ID has the
// OLD lifetime, so EventsResume returns everything (the OLD-lifetime
// rows in events get redelivered too — client de-dups them via its
// in-memory cache).
func TestRunEvents_LifetimeMismatchReplaysAll(t *testing.T) {
	srv, store, _ := runEventsHarness(t)
	seedEvent(t, store, "R1", "schedule_start", "lifeA", 1)
	seedEvent(t, store, "R1", "task_start", "lifeA", 2)

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/R1/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Last-Event-ID", "lifeOLD-99")
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "id: lifeA-1\n") || !strings.Contains(body, "id: lifeA-2\n") {
		t.Fatalf("lifetime mismatch should replay all; got: %s", body)
	}
}

// TestRunEvents_LiveTailUsesTaggedID: a live event injected via
// LiveSink (no events-table row yet, mid-stream) ends up framed with
// the same "<lifetime>-<seq>" id form as the replay path.
func TestRunEvents_LiveTailUsesTaggedID(t *testing.T) {
	srv, _, ls := runEventsHarness(t)

	// Open the request with a real http.Server so the response body
	// streams piecemeal — httptest.ResponseRecorder buffers, which
	// breaks live-tail tests.
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

	// Stream-scan the body inline so we observe frames AS they arrive,
	// not after the body closes. Each SSE frame ends with a blank line;
	// we accumulate the current frame's lines until a blank, then test.
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
				if strings.Contains(frame, "id: lifeLIVE-7") {
					got <- frame
					return
				}
				continue
			}
			cur = append(cur, line)
		}
		got <- ""
	}()

	// Give the handler a beat to subscribe before injecting.
	time.Sleep(50 * time.Millisecond)
	line := []byte(`{"time":"2026-05-10T12:00:00Z","msg":"live","entry_type":"task_start","run_id":"R1","seq":7,"lifetime":"lifeLIVE"}`)
	ls.Inject("R1", line)

	select {
	case frame := <-got:
		if frame == "" {
			t.Fatal("live tail did not deliver tagged frame")
		}
		if !strings.Contains(frame, "data: {") || !strings.Contains(frame, `"seq":7`) {
			t.Fatalf("live frame missing seq in payload: %s", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live frame")
	}
}

// TestEventsResume_LifetimeFilter: direct unit on the cursor SQL. A
// run with rows from two lifetimes returns the correct subset for each
// resume case — same-lifetime skip-seen, cross-lifetime full-replay.
func TestEventsResume_LifetimeFilter(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// 3 events across 2 lifetimes — simulates a producer restart mid-run.
	seedEvent(t, store, "R1", "schedule_start", "lifeA", 1)
	seedEvent(t, store, "R1", "task_start", "lifeA", 2)
	seedEvent(t, store, "R1", "task_start", "lifeB", 1)

	// Case 1: client has lifeA-1, wants the rest. Should get lifeA-2 +
	// lifeB-1 (everything in lifeB regardless of seq value).
	rows, err := store.EventsResume("R1", "lifeA", 1)
	if err != nil {
		t.Fatalf("EventsResume(lifeA,1): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	gotKeys := []string{}
	for _, r := range rows {
		gotKeys = append(gotKeys, fmt.Sprintf("%s-%d", r.Lifetime, r.Seq))
	}
	if !contains(gotKeys, "lifeA-2") || !contains(gotKeys, "lifeB-1") {
		t.Fatalf("wrong rows: %v", gotKeys)
	}

	// Case 2: client has stale lifeOLD-99 — gets everything.
	rows, err = store.EventsResume("R1", "lifeOLD", 99)
	if err != nil {
		t.Fatalf("EventsResume(lifeOLD,99): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("stale lifetime should give full replay, got %d", len(rows))
	}

	// Case 3: fresh client (empty lifetime, seq=0) — gets everything.
	rows, err = store.EventsResume("R1", "", 0)
	if err != nil {
		t.Fatalf("EventsResume(empty): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("fresh client should get all events, got %d", len(rows))
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestTaggerInjectsSeqAndLifetime: state.Tagger sits at the top of the
// slog chain. Every record it sees should emerge with seq + lifetime
// attrs on the downstream handler. Process-monotonic seq means
// successive records get strictly increasing values; lifetime is the
// same across calls within the process.
func TestTaggerInjectsSeqAndLifetime(t *testing.T) {
	collect := &collectingHandler{}
	tagger := state.NewTagger(collect)
	logger := slog.New(tagger)

	logger.Info("a")
	logger.Info("b")
	logger.Info("c")

	if len(collect.records) != 3 {
		t.Fatalf("want 3 records, got %d", len(collect.records))
	}
	// All three records share the same lifetime.
	lifetimes := map[string]bool{}
	seqs := []int64{}
	for _, r := range collect.records {
		var lt string
		var seq int64
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "seq":
				seq = a.Value.Int64()
			case "lifetime":
				lt = a.Value.String()
			}
			return true
		})
		lifetimes[lt] = true
		seqs = append(seqs, seq)
	}
	if len(lifetimes) != 1 {
		t.Fatalf("lifetime should be constant within the process, got %d distinct: %v",
			len(lifetimes), lifetimes)
	}
	if seqs[0] >= seqs[1] || seqs[1] >= seqs[2] {
		t.Fatalf("seqs should be strictly increasing, got %v", seqs)
	}
}

// collectingHandler is a tiny test fake: records every record it gets,
// no encoding, no filtering. Used to inspect what Tagger forwards.
type collectingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *collectingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (c *collectingHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.records = append(c.records, r)
	c.mu.Unlock()
	return nil
}
func (c *collectingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *collectingHandler) WithGroup(_ string) slog.Handler      { return c }

// TestRetag_PreservesOriginAndAssignsLocalSeq: the ingest path's worker
// events arrive carrying the WORKER's seq+lifetime. Retag stamps the
// PRODUCER's seq+lifetime on top, and tucks the worker's into
// origin_seq/origin_lifetime so operators can still trace back.
func TestRetag_PreservesOriginAndAssignsLocalSeq(t *testing.T) {
	in := []byte(`{"entry_type":"task_start","run_id":"R","seq":42,"lifetime":"workerLT"}`)
	out, newSeq := state.Retag(in)
	if newSeq == 0 {
		t.Fatal("Retag should assign a non-zero seq")
	}
	// Decode and inspect.
	ev, ok := state.DecodeEvent(out)
	if !ok {
		t.Fatalf("Retag produced an undecodable line: %s", out)
	}
	if ev.Seq != newSeq {
		t.Fatalf("ev.Seq=%d, want %d", ev.Seq, newSeq)
	}
	if ev.Lifetime == "" || ev.Lifetime == "workerLT" {
		t.Fatalf("ev.Lifetime should be re-stamped, got %q", ev.Lifetime)
	}
	// origin_* fields are on the raw map, not the typed Event — check via
	// the payload bytes.
	raw := string(out)
	if !strings.Contains(raw, `"origin_seq":42`) {
		t.Errorf("missing origin_seq: %s", raw)
	}
	if !strings.Contains(raw, `"origin_lifetime":"workerLT"`) {
		t.Errorf("missing origin_lifetime: %s", raw)
	}
}

// TestRetag_NotJSONPassthrough: malformed input → returns the line
// unchanged + seq=0. Ingest path falls through to DecodeEvent, which
// counts the line as dropped.
func TestRetag_NotJSONPassthrough(t *testing.T) {
	in := []byte("not even json")
	out, seq := state.Retag(in)
	if seq != 0 {
		t.Errorf("malformed input should have seq=0, got %d", seq)
	}
	if string(out) != string(in) {
		t.Errorf("malformed input should pass through unchanged: in=%s out=%s", in, out)
	}
}
