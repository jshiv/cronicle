package state

import (
	"encoding/json"
	"testing"
	"time"
)

// forEachRun enqueues and projects a run of list → review[*] → report[*]
// → summary where review[15] failed (so report[15] never ran).
func forEachRun(t *testing.T) *Store {
	t.Helper()
	s := newQueueTestStore(t)
	payload := `{"Name":"prs","RunID":"R1","Tasks":[
		{"Name":"list","Depends":null},
		{"Name":"review","Depends":null,"ForEach":{"Expr":"task.list.output","Source":"list"}},
		{"Name":"report","Depends":["review"],"ForEach":{"Expr":"task.list.output","Source":"list"}},
		{"Name":"summary","Depends":["report"]}
	]}`
	if err := s.Enqueue("R1", "prs", []byte(payload)); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Claim("W_A", time.Minute)
	items := `[{"key":"12","value":{"sha":"a1"}},{"key":"15","value":{"sha":"b2"}}]`
	expanded := func(task string) string {
		b, _ := json.Marshal(map[string]any{
			"time": "2026-05-10T12:00:01Z", "entry_type": "task_expanded", "run_id": "R1",
			"schedule": "prs", "task": task, "for_each_items": items,
		})
		return string(b)
	}
	applyLine(t, s, `{"time":"2026-05-10T12:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"prs","tasks":["list","review","report","summary"]}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:01Z","entry_type":"shell_run","run_id":"R1","schedule":"prs","task":"list","exit":0,"success":true}`)
	applyLine(t, s, expanded("review"))
	applyLine(t, s, expanded("report"))
	applyLine(t, s, `{"time":"2026-05-10T12:00:02Z","entry_type":"shell_run","run_id":"R1","schedule":"prs","task":"review[12]","exit":0,"success":true}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:02Z","entry_type":"shell_run","run_id":"R1","schedule":"prs","task":"review[15]","exit":1,"success":false}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:03Z","entry_type":"shell_run","run_id":"R1","schedule":"prs","task":"report[12]","exit":0,"success":true}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:03Z","entry_type":"for_each_complete","run_id":"R1","schedule":"prs","task":"review","success":false}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:04Z","entry_type":"schedule_complete","run_id":"R1","schedule":"prs","success":false}`)
	if err := s.Ack("R1", "W_A", false, "review[15] failed"); err != nil {
		t.Fatal(err)
	}
	return s
}

type bakedTask struct {
	Name    string
	Depends []string
	ForEach *struct {
		Source string
		Items  []struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		Skip []string
	}
}

func payloadTasks(t *testing.T, s *Store, runID string) map[string]bakedTask {
	t.Helper()
	var stored string
	if err := s.db.QueryRow(`SELECT payload FROM jobs WHERE run_id = ?`, runID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var raw struct{ Tasks []bakedTask }
	if err := json.Unmarshal([]byte(stored), &raw); err != nil {
		t.Fatal(err)
	}
	out := map[string]bakedTask{}
	for _, tk := range raw.Tasks {
		out[tk.Name] = tk
	}
	return out
}

// TestTaskExpanded_RecordsItemsAndRows: the expansion is stored with the
// run, each item gets a queued row up front, and the for_each task's row
// finishes with for_each_complete.
func TestTaskExpanded_RecordsItemsAndRows(t *testing.T) {
	s := forEachRun(t)
	var items string
	if err := s.db.QueryRow(`SELECT items FROM task_expansions WHERE run_id = 'R1' AND task = 'review'`).Scan(&items); err != nil {
		t.Fatalf("expansion not recorded: %v", err)
	}
	status := func(name string) string {
		var st string
		_ = s.db.QueryRow(`SELECT status FROM tasks WHERE run_id = 'R1' AND name = ?`, name).Scan(&st)
		return st
	}
	if got := status("report[15]"); got != StatusQueued {
		t.Errorf("report[15] = %q, want a queued row from the expansion", got)
	}
	if got := status("review"); got != StatusFailed {
		t.Errorf("review = %q, want failed after for_each_complete", got)
	}
}

// TestRetryFailed_ReusesRecordedItems: retrying the failed run reruns
// review and report over the recorded items, skipping the ones that
// succeeded, and doesn't rerun the source.
func TestRetryFailed_ReusesRecordedItems(t *testing.T) {
	s := forEachRun(t)
	if _, err := s.RetryFailed("R1", "R2"); err != nil {
		t.Fatalf("RetryFailed: %v", err)
	}
	tasks := payloadTasks(t, s, "R2")
	if _, ok := tasks["list"]; ok {
		t.Fatal("the source succeeded and should not rerun")
	}
	review := tasks["review"]
	if review.ForEach == nil || len(review.ForEach.Items) != 2 {
		t.Fatalf("review should carry the recorded items: %+v", review.ForEach)
	}
	if got := review.ForEach.Skip; len(got) != 1 || got[0] != "12" {
		t.Errorf("review skip = %v, want [12]", got)
	}
	if got := tasks["report"].ForEach.Skip; len(got) != 1 || got[0] != "12" {
		t.Errorf("report skip = %v, want [12]", got)
	}
	if string(review.ForEach.Items[0].Value) != `{"sha":"a1"}` {
		t.Errorf("item value changed: %s", review.ForEach.Items[0].Value)
	}
}

// TestRetryTask_OneItem: retrying review[15] reruns just that item and its
// paired report item, plus the fan-in that follows.
func TestRetryTask_OneItem(t *testing.T) {
	s := forEachRun(t)
	keep := map[string]bool{"review[15]": true, "report": true, "summary": true}
	if _, err := s.RetryTask("R1", "review[15]", keep, "R3"); err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	tasks := payloadTasks(t, s, "R3")
	if len(tasks) != 3 {
		t.Fatalf("want review, report, summary; got %v", tasks)
	}
	for _, name := range []string{"review", "report"} {
		fe := tasks[name].ForEach
		if fe == nil || len(fe.Skip) != 1 || fe.Skip[0] != "12" {
			t.Errorf("%s should run only key 15: %+v", name, fe)
		}
	}
}
