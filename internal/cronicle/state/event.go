package state

import (
	"encoding/json"
	"time"
)

// Event is the typed projection of one slog record carrying an entry_type.
// Wire format on the worker→producer path is the same JSON line that
// already lands in cronicle.jsonl, so the same bytes feed both the file
// log and the projection. Unknown fields are ignored — adding new attrs
// to a slog call doesn't require a wire-protocol bump.
type Event struct {
	Time      time.Time `json:"time"`
	Msg       string    `json:"msg,omitempty"`
	EntryType string    `json:"entry_type"`
	RunID     string    `json:"run_id"`
	Schedule  string    `json:"schedule"`
	Task      string    `json:"task,omitempty"`
	Source    string    `json:"source,omitempty"`
	WorkerID  string    `json:"worker_id,omitempty"`

	// schedule_start
	Tasks []string `json:"tasks,omitempty"`
	DAG   string   `json:"dag,omitempty"`

	// task_start
	Attempt int `json:"attempt,omitempty"`

	// shell_run / agent_run terminal events
	Exit       *int     `json:"exit,omitempty"`
	DurationMs *int64   `json:"duration_ms,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	Success    *bool    `json:"success,omitempty"`
	Error      string   `json:"error,omitempty"`
	Command    string   `json:"command,omitempty"`
	Model      string   `json:"model,omitempty"`

	// schedule_complete
	TaskCount int `json:"task_count,omitempty"`

	// SSE de-dup key: minted by state.Tagger at the top of the slog chain
	// (or by Retag() on the ingest path). Zero/empty when an Event is
	// constructed programmatically without going through the chain — the
	// projection writes NULL columns in that case and replay treats them
	// as "deliver always".
	Seq      int64  `json:"seq,omitempty"`
	Lifetime string `json:"lifetime,omitempty"`

	// raw is the serialized JSON record this Event was decoded from. Stored
	// verbatim in events.payload so /v1/runs/{id}/events can replay the
	// original line shape — including any fields the typed Event drops.
	raw []byte
}

// DecodeEvent parses a single JSONL record into an Event. Returns ok=false
// when the record has no entry_type (lifecycle log lines we don't project)
// or no run_id (the projection key — without it the event isn't actionable).
func DecodeEvent(line []byte) (Event, bool) {
	var e Event
	if err := json.Unmarshal(line, &e); err != nil {
		return Event{}, false
	}
	if e.EntryType == "" {
		return Event{}, false
	}
	if e.RunID == "" {
		// Some events (heartbeat, config reload, listener startup) don't
		// belong to any specific run. Drop them — they're for humans, not
		// the projection.
		return Event{}, false
	}
	e.raw = append([]byte(nil), line...)
	return e, true
}

// Raw returns the original JSON line if the Event was constructed via
// DecodeEvent. Empty when the Event was built programmatically.
func (e Event) Raw() []byte { return e.raw }
