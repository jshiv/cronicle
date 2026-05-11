package cronicle

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
)

// rehydrateRecord turns one JSONL line shipped by a worker's
// eventShipper (worker_event_ship.go:Handle) back into the slog.Record
// that produced it. Used by the runner's /v1/events ingest path so
// worker-side records flow through the runner's LiveSink encoder —
// SSE subscribers then see one consistent --live-format regardless of
// whether the work happened locally or on a worker.
//
// The wire shape is whatever the worker's shipper writes: a flat JSON
// object with `time`, `level`, `msg`, plus all slog attrs as top-level
// keys. We reverse that here.
//
// Returns ok=false on malformed JSON or if `msg`/`time` are missing.
// Caller drops the line in that case — the projection path already
// counted it as accepted, but the live stream simply skips frames we
// can't reconstruct rather than emitting garbage.
//
// Numeric values: workers ship Go integers as JSON numbers. To keep
// Int64 vs Float64 kind fidelity (renderers call a.Value.Int64() on
// integer attrs like `exit` and `duration_ms`), we use json.Number
// and probe for integer-ness before falling back to float64.
//
// Slice values: when every element is a string we reconstruct the
// []string the renderer expects (e.g. schedule_start's `tasks` attr,
// agent_run_start's `skills_available`). Mixed-type slices fall back
// to a JSON-stringified blob — those don't appear in current cronicle
// records but the path stays safe.
func rehydrateRecord(line []byte) (slog.Record, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return slog.Record{}, false
	}

	var t time.Time
	if v, ok := m["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			t = parsed
		}
	}

	lvl := slog.LevelInfo
	if v, ok := m["level"].(string); ok {
		switch strings.ToUpper(v) {
		case "DEBUG":
			lvl = slog.LevelDebug
		case "INFO":
			lvl = slog.LevelInfo
		case "WARN", "WARNING":
			lvl = slog.LevelWarn
		case "ERROR":
			lvl = slog.LevelError
		}
	}

	msg, _ := m["msg"].(string)

	rec := slog.NewRecord(t, lvl, msg, 0)

	// AddAttrs in a stable-ish order: deterministic key iteration via
	// sorted keys keeps test assertions easier and the rendered output
	// reproducible. Map iteration in Go is randomized; renderers don't
	// care about order today but byte-for-byte test assertions would.
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "time" || k == "level" || k == "msg" {
			continue
		}
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		rec.AddAttrs(jsonValueToAttr(k, m[k]))
	}
	return rec, true
}

// jsonValueToAttr maps a json.Decoder UseNumber result back to the
// slog kind a renderer would expect for that key. We can't infer the
// original kind from the JSON alone for numbers (a Go int and float
// look identical when small), so we prefer Int64 when the json.Number
// parses cleanly as one — that's what every cronicle int attr looks
// like on the wire.
func jsonValueToAttr(k string, v any) slog.Attr {
	switch x := v.(type) {
	case string:
		return slog.String(k, x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return slog.Int64(k, i)
		}
		if f, err := x.Float64(); err == nil {
			return slog.Float64(k, f)
		}
		return slog.String(k, x.String())
	case bool:
		return slog.Bool(k, x)
	case nil:
		return slog.String(k, "")
	case []any:
		if strs, ok := allStringSlice(x); ok {
			// slog.Any preserves []string under .Any() — renderers like
			// renderScheduleStart and renderAgentRunStart type-assert
			// directly, so the kind must match exactly.
			return slog.Any(k, strs)
		}
		b, _ := json.Marshal(x)
		return slog.String(k, string(b))
	case map[string]any:
		b, _ := json.Marshal(x)
		return slog.String(k, string(b))
	default:
		return slog.Any(k, v)
	}
}

func allStringSlice(xs []any) ([]string, bool) {
	out := make([]string, len(xs))
	for i, x := range xs {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

// sortStrings is a tiny inline insertion sort. Not using sort.Strings
// to keep the rehydrate path's working set small and avoid the import
// for what's almost always <10 keys per record.
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
