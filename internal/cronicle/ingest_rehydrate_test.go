package cronicle

import (
	"log/slog"
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
