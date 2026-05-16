package cronicle

import (
	"log/slog"
	"strings"
	"testing"
)

// TestRehydrateRecord_RoundtripIntKind: the renderers read integer
// attrs via a.Value.Int64() — if rehydrate produces Float64Kind for
// JSON numbers, .Int64() panics. This test pins the contract: any
// JSON number that parses as int64 must come back as Int64Kind.
func TestRehydrateRecord_RoundtripIntKind(t *testing.T) {
	line := []byte(`{"time":"2026-05-11T12:00:00Z","level":"INFO","msg":"shell complete","entry_type":"shell_run","run_id":"R1","schedule":"s","task":"t","exit":0,"duration_ms":1234,"success":true}`)
	rec, ok := rehydrateRecord(line)
	if !ok {
		t.Fatal("rehydrate: ok=false")
	}
	got := map[string]slog.Kind{}
	rec.Attrs(func(a slog.Attr) bool {
		got[a.Key] = a.Value.Kind()
		return true
	})
	for _, k := range []string{"exit", "duration_ms"} {
		if got[k] != slog.KindInt64 {
			t.Errorf("attr %q: kind=%v, want Int64", k, got[k])
		}
	}
	if got["success"] != slog.KindBool {
		t.Errorf("attr success: kind=%v, want Bool", got["success"])
	}
	if got["task"] != slog.KindString {
		t.Errorf("attr task: kind=%v, want String", got["task"])
	}
}

// TestRehydrateRecord_StringSliceKind: schedule_start ships a
// "tasks":["a","b"] attr that renderScheduleStart type-asserts as
// []string via a.Value.Any().([]string). The rehydrate must preserve
// that shape — anything else makes the DAG line render as a generic
// "tasks=[a b]" instead of the pretty list.
func TestRehydrateRecord_StringSliceKind(t *testing.T) {
	line := []byte(`{"time":"2026-05-11T12:00:00Z","level":"INFO","msg":"schedule started","entry_type":"schedule_start","run_id":"R1","schedule":"daily","tasks":["a","b","c"]}`)
	rec, ok := rehydrateRecord(line)
	if !ok {
		t.Fatal("rehydrate: ok=false")
	}
	var tasks []string
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key != "tasks" {
			return true
		}
		v, ok := a.Value.Any().([]string)
		if !ok {
			t.Fatalf("tasks attr: underlying type = %T, want []string", a.Value.Any())
		}
		tasks = v
		return false
	})
	if len(tasks) != 3 || tasks[0] != "a" || tasks[2] != "c" {
		t.Fatalf("tasks: %v", tasks)
	}
}

// TestRehydrateRecord_MalformedReturnsFalse: garbage in → ok=false,
// caller drops the frame. We don't want partial records reaching SSE.
func TestRehydrateRecord_MalformedReturnsFalse(t *testing.T) {
	if _, ok := rehydrateRecord([]byte(`{not valid`)); ok {
		t.Fatal("expected ok=false for malformed JSON")
	}
}

// TestRenderAgentRunFooter_IntCostUsd: regression — when an agent
// failed before any token usage (e.g. missing API key), cost_usd lands
// on the wire as JSON `0`, which rehydrate decodes as Int64. The footer
// renderer previously called a.Value.Float64() unconditionally and
// panicked, aborting the rest of the worker→producer ingest batch and
// leaving the run row stuck on status=running. valueFloat64 must tolerate
// either kind.
func TestRenderAgentRunFooter_IntCostUsd(t *testing.T) {
	line := []byte(`{"time":"2026-05-16T17:04:00Z","level":"ERROR","msg":"agent run failed","entry_type":"agent_run","run_id":"R1","schedule":"s","task":"t","cost_usd":0,"duration_ms":1318,"input_tokens":0,"output_tokens":0,"cache_read":0,"stop_reason":"","success":false,"error":"no creds"}`)
	rec, ok := rehydrateRecord(line)
	if !ok {
		t.Fatal("rehydrate: ok=false")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("renderAgentRunFooter panicked: %v", r)
		}
	}()
	var buf strings.Builder
	if err := renderAgentRunFooter(&buf, rec); err != nil {
		t.Fatalf("renderAgentRunFooter: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("renderAgentRunFooter produced no output")
	}
}
