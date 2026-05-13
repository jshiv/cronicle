package state

import (
	"context"
	"time"
)

// Backend is the seam between the cronicle binary and the storage layer
// that holds projection rows, the queue, and runtime control state. The
// in-tree implementation is *Store (modernc.org/sqlite under .cronicle/
// state.db); cronicle-infra (and any future cluster runner) plugs in
// its own implementation by satisfying this interface.
//
// The interface is composed from small role interfaces below — consumers
// can embed just the slice they need (for example, a worker that only
// claims jobs takes JobQueue rather than the full Backend). The Backend
// alias is the union, useful for the global StateStore() accessor and
// for the listener which touches every facet.
type Backend interface {
	EventSink
	RunReader
	ControlReader
	ControlWriter
	JobQueue
	WorkerRegistry
	RetryAPI
	ControlChannel

	// Close releases the underlying connection. Implementations must be
	// safe to call multiple times; the in-tree *Store guards against it.
	Close() error
}

// EventSink folds slog-derived events into the projection. Called by
// the slog Sink handler on every structural event (schedule_*, task_*,
// shell_run, agent_run). Idempotent on row-level keys; the Sink fan-out
// already filters per-token noise.
type EventSink interface {
	Apply(e Event) error
}

// RunReader serves the read-only run/projection endpoints.
type RunReader interface {
	GetRun(runID string) (Run, error)
	ListRuns(f ListFilter) ([]Run, error)
	CountRuns() (int, error)
}

// ControlReader is the hot path for the cron tick + DAG walker. Each
// call is a single-row PK lookup; implementations should cache or
// memoize when the underlying store can be slow (Postgres over the
// network).
type ControlReader interface {
	IsDrained() (bool, error)
	IsSchedulePaused(name string) (bool, error)
	IsRunPaused(runID string) (bool, error)
	IsTaskSkipped(schedule, task string) (bool, error)
	IsTaskCanceledInRun(runID, taskName string) (bool, error)
	GetRunnerState() (RunnerState, error)
	GetScheduleState(name string) (ScheduleState, error)
	GetRunState(runID string) (RunState, error)
	GetTaskState(schedule, task string) (TaskState, error)
	ListPausedSchedules() ([]ScheduleState, error)
	ListSkippedTasksForSchedule(schedule string) ([]TaskState, error)
}

// ControlWriter mutates the runtime-control rows. Wired from the
// listener handlers; on a multi-runner deployment these writes need to
// be globally visible (Postgres LISTEN/NOTIFY or polling, depending on
// the implementation).
type ControlWriter interface {
	SetDrained(actor, reason string) error
	ClearDrained(actor string) error
	SetSchedulePaused(name, actor, reason string) error
	ClearSchedulePaused(name, actor string) error
	PauseRun(runID, actor, reason string) error
	ResumeRun(runID, actor string) error
	SetTaskSkipped(schedule, task, actor, reason string) error
	ClearTaskSkipped(schedule, task, actor string) error
	Cancel(runID string) (CancelResult, error)
	CancelTask(runID, taskName string, cascade []string) (CancelTaskResult, error)
}

// JobQueue is the producer/worker queue. In single-node mode the
// in-process channel adapter (queue_self.go) calls Enqueue; in
// distributed mode the listener's POST /v1/events ingest handler does.
// Workers call Claim/Heartbeat/Ack via the HTTP worker (worker_http.go)
// or directly in-process.
type JobQueue interface {
	Enqueue(runID, schedule string, payload []byte) error
	Claim(workerID string, visibility time.Duration) (Job, error)
	Ack(runID, workerID string, success bool, errMsg string) error
	Heartbeat(runID, workerID string, visibility time.Duration) error
	ReapExpired() (int, error)
	WaitForJob(ctx context.Context, timeout time.Duration) bool
	CountJobsByStatus(status string) (int, error)
}

// WorkerRegistry tracks live workers for the operator's "who's up?"
// view. Claimed jobs auto-upsert via Claim(); UpsertWorker is the
// explicit registration path used by long-poll endpoints to keep the
// row warm during idle stretches.
type WorkerRegistry interface {
	UpsertWorker(workerID, host string) error
	ListWorkers() ([]Worker, error)
}

// RetryAPI re-enqueues terminal runs with various inclusion shapes.
// Caller mints the new run_id so producer-side ID format is preserved.
type RetryAPI interface {
	Retry(runID, newRunID string) (RetryResult, error)
	RetryFailed(runID, newRunID string) (RetryResult, error)
	RetryTask(runID, taskName string, keep map[string]bool, newRunID string) (RetryResult, error)
}

// ControlChannel is the worker-facing SSE control plane. Producers push
// cancel / cancel_task / drain hints; subscribed workers receive them
// and act (e.g., cancel the per-run ctx). Drops if the worker chan is
// full — control hints are best-effort, the projection is authoritative.
type ControlChannel interface {
	Subscribe(workerID string) (<-chan ControlMsg, func())
	PushControl(workerID string, msg ControlMsg) bool
}

// Compile-time guarantee that the in-tree *Store satisfies Backend.
// External implementations (cronicle-infra's PostgresStore) embed or
// reuse role interfaces and add the same assertion.
var _ Backend = (*Store)(nil)
