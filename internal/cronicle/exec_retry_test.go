package cronicle

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// runAndCountAttempts wires up a failing shell task with the supplied
// Retry config and returns the number of times task.Exec actually ran.
// The "command" appends to a counter file on each attempt and exits
// non-zero so try.Do treats it as failed and decides whether to retry.
func runAndCountAttempts(t *testing.T, retry *Retry) int {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	// Each attempt appends one byte to the counter file then exits 1.
	// File length after task.Execute returns equals the attempt count.
	cmd := []string{"sh", "-c",
		fmt.Sprintf("printf x >> %s; exit 1", counter)}

	task := &Task{
		Name:         "retrycount",
		Command:      cmd,
		Path:         dir,
		CroniclePath: dir,
		ScheduleName: "t",
		Retry:        retry,
	}
	// task.Execute logs attempt info via slog; suppress noise during tests.
	SetupLogging(LogFormatText)

	_, _ = task.Execute(time.Now())

	body, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return len(body)
}

// TestTaskRetry_NoRetryConfigured: a task with no Retry block runs
// exactly once. Establishes the baseline so the Count cases below
// can assert deltas rather than absolute numbers.
func TestTaskRetry_NoRetryConfigured(t *testing.T) {
	if got := runAndCountAttempts(t, nil); got != 1 {
		t.Errorf("no Retry: got %d attempts, want 1 (no retries)", got)
	}
}

// TestTaskRetry_CountMatchesDocumentedSemantics locks in the M4 fix.
//
// Retry.Count is documented as "number of retry attempts after first
// attempt", so Count=N must produce N+1 total runs. Previously
// `attempt < retryCount` short-circuited one attempt early, giving
// Count=3 only 3 total attempts (2 retries) instead of 4 (3 retries).
//
// Backwards-incompatible behavior change: HCL configs that set
// `count = N` will now see one additional invocation per failing run.
func TestTaskRetry_CountMatchesDocumentedSemantics(t *testing.T) {
	cases := []struct {
		count       int
		wantTotal   int
		description string
	}{
		{count: 1, wantTotal: 2, description: "1 retry → 2 total"},
		{count: 2, wantTotal: 3, description: "2 retries → 3 total"},
		{count: 3, wantTotal: 4, description: "3 retries → 4 total"},
	}
	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			got := runAndCountAttempts(t, &Retry{Count: tc.count})
			if got != tc.wantTotal {
				t.Errorf("Count=%d: got %d total attempts, want %d",
					tc.count, got, tc.wantTotal)
			}
		})
	}
}
