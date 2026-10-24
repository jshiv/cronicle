package cronicle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatih/color"
	"github.com/go-git/go-git/v6"
	"github.com/hashicorp/hcl/v2/hclparse"
)

// parseConfig decodes HCL the way cronicle.hcl is loaded and validates it.
func parseConfig(t *testing.T, src string) (*Config, error) {
	t.Helper()
	conf, diags := ParseBytes([]byte(src), "cronicle.hcl", hclparse.NewParser())
	if diags.HasErrors() {
		return conf, diags
	}
	return conf, conf.Init(t.TempDir())
}

func TestForEach_Parse(t *testing.T) {
	conf, err := parseConfig(t, `
schedule "s" {
  task "list" { command = ["echo", "{}"] }
  task "each" {
    for_each     = { for pr in task.list.output : pr.number => pr }
    max_parallel = 2
    command      = ["review", each.key, each.value.sha, "${each.value.meta["x"]}"]
  }
  task "static" {
    for_each = toset(["a", "b"])
    command  = ["echo", each.key]
  }
}`)
	if err != nil {
		t.Fatal(err)
	}
	tasks := conf.Schedules[0].TaskMap()
	fe := tasks["each"].ForEach
	if fe == nil || fe.Source != "list" || fe.MaxParallel != 2 || !strings.HasPrefix(fe.Expr, "{ for pr in task.list.output") {
		t.Fatalf("bad ForEach: %+v", fe)
	}
	want := []string{"review", "${each.key}", "${each.value.sha}", "${each.value.meta.x}"}
	if got := tasks["each"].Command; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("command = %q, want placeholders %q", got, want)
	}
	if st := tasks["static"].ForEach; st == nil || st.Source != "" {
		t.Errorf("static for_each: %+v", st)
	}
	if tasks["list"].ForEach != nil {
		t.Error("a task without for_each got one")
	}
}

func TestForEach_ParseErrors(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`task "a" { for_each = task.x.stdout
		  command = ["echo"] }
		  task "x" { command = ["echo"] }`, `use task.x.output`},
		{`task "a" { for_each = merge(task.x.output, task.y.output)
		  command = ["echo"] }`, `a collection can come from one task`},
		{`task "a" { for_each = [each.key]
		  command = ["echo"] }`, `only available inside the task`},
		{`task "a" { for_each = task.missing.output
		  command = ["echo"] }`, `has no task "missing"`},
		{`task "a" { for_each = task.a.output
		  command = ["echo"] }`, `can't read its own output`},
		{`task "x" { for_each = toset(["k"])
		  command = ["echo"] }
		  task "a" { for_each = task.x.output
		  command = ["echo"] }`, `is itself a for_each task`},
		{`task "a" { max_parallel = 2
		  command = ["echo"] }`, `max_parallel needs for_each`},
		{`task "a" { command = ["echo", each.key] }`, `has no for_each`},
		{`task "a" { for_each = toset(["k"])
		  command = ["echo", each.value, each.value.x] }`, `used both whole and by field`},
		{`task "a" { for_each = toset(["k"])
		  command = ["echo", each.item] }`, `only each.key and each.value`},
		{`task "a" { for_each = 3
		  command = ["echo"] }`, `must be a map, or set of strings, and you have provided a value of type number`},
		{`task "a" { for_each = ["us", "eu"]
		  command = ["echo"] }`, `convert a list of strings with toset(...)`},
		{`task "a" { for_each = toset(["k"])
		  command = ["echo"] }
		  task "b" { depends = ["a[k]"]
		  command = ["echo"] }`, `depend on the for_each task "a" instead`},
		{`task "a[1]" { command = ["echo"] }`, `names can't contain [ or ]`},
		{`task "a" { for_each = toset(["k"])
		  command = ["echo"]
		  repo {
		    url      = "https://example.com/r.git"
		    password = each.value
		  }
		  }`, `not its credentials`},
		{`task "a" { command = ["echo"]
		  repo {
		    url    = "https://example.com/r.git"
		    branch = each.key
		  }
		  }`, `has no for_each`},
	} {
		// The cases are written compactly; open each block on its own line.
		body := strings.ReplaceAll(strings.ReplaceAll(tc.body, `" { `, "\" {\n"), "] }", "]\n}")
		_, err := parseConfig(t, "schedule \"s\" {\n"+body+"\n}")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want error containing %q, got %v\n%s", tc.want, err, tc.body)
		}
	}
}

// TestForEach_CollectionRules: Terraform's for_each rules, exactly.
func TestForEach_CollectionRules(t *testing.T) {
	eval := func(expr, output string) ([]ForEachItem, error) {
		return (&ForEach{Expr: expr, Source: "list"}).evaluate(map[string]string{"list": output})
	}
	keys := func(expr, output string) string {
		t.Helper()
		items, err := eval(expr, output)
		if err != nil {
			t.Errorf("%s over %s: %v", expr, output, err)
		}
		return strings.Join(forEachKeys(items), ",")
	}
	// Maps and objects: one item per key, in sorted order.
	if got := keys("task.list.output", `{"b": {"n": 2}, "a": {"n": 1}}`); got != "a,b" {
		t.Errorf("object keys: %s", got)
	}
	// Sets of strings: sorted, duplicates collapsed, each.value == each.key.
	if got := keys("toset(task.list.output)", `["us", "eu", "us"]`); got != "eu,us" {
		t.Errorf("toset: %s", got)
	}
	items, _ := eval("toset(task.list.output)", `["us"]`)
	if string(items[0].Value) != `"us"` {
		t.Errorf("set item value: %s", items[0].Value)
	}
	// A for expression keys a list of objects.
	if got := keys("{ for pr in task.list.output : pr.number => pr }", `[{"number": 7}, {"number": 9}]`); got != "7,9" {
		t.Errorf("keyed list: %s", got)
	}
	if got := keys("task.list.output.prs", `{"prs": {}}`); got != "" {
		t.Errorf("empty collection should expand to nothing: %s", got)
	}

	// Terraform's functions.
	for expr, want := range map[string]string{
		`toset([for i in range(3) : tostring(i)])`:                                       "0,1,2",
		`merge({ main = "HEAD" }, { for pr in task.list.output : pr.number => pr.sha })`: "12,main",
		`toset([for s in task.list.output : upper(replace(s.sha, "/[0-9]/", ""))])`:      "ABC",
		`{ for k, v in zipmap(["x", "y"], [1, 2]) : k => v if v > 1 }`:                   "y",
		`toset(try(task.list.output.missing, ["fallback"]))`:                             "fallback",
		`setsubtract(toset(["a", "b", "c"]), toset(["b"]))`:                              "a,c",
	} {
		if got := keys(expr, `[{"number": 12, "sha": "abc123"}]`); got != want {
			t.Errorf("%s = %s, want %s", expr, got, want)
		}
	}

	for _, tc := range []struct{ expr, out, want string }{
		// A list is not a set.
		{"task.list.output", `["a", "b"]`, "provided a value of type tuple; convert a list of strings with toset(...)"},
		{"tolist(task.list.output)", `["a"]`, "provided a value of type list of string"},
		{"toset(task.list.output)", `[1, 2]`, "set containing type number"},
		{"task.list.output", `[{"number": 7}]`, "build a map with a for expression"},
		{"task.list.output", `not json`, "not JSON"},
		{"task.list.output", ``, "printed nothing"},
		{"task.list.output", `null`, "null"},
		{"toset(task.list.output)", `["a[1]"]`, "brackets"},
		{"toset(task.list.output)", `[` + manyKeys(maxForEachItems+1) + `]`, "over the limit"},
	} {
		if _, err := eval(tc.expr, tc.out); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s over %.30q: want %q, got %v", tc.expr, tc.out, tc.want, err)
		}
	}
	if _, err := (&ForEach{Expr: "task.list.output", Source: "list"}).evaluate(map[string]string{}); err == nil ||
		!strings.Contains(err.Error(), "did not run in this run") {
		t.Errorf("missing source output: %v", err)
	}
}

func manyKeys(n int) string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("%q", fmt.Sprint(i))
	}
	return strings.Join(keys, ",")
}

func TestForEach_SubstituteEach(t *testing.T) {
	item := ForEachItem{Key: "12", Value: json.RawMessage(`{"sha":"a1","n":12345678901234567890,"ok":true,"tags":["x","y"],"meta":{"a":1}}`)}
	for in, want := range map[string]string{
		"${each.key}":            "12",
		"pr-${each.key}.md":      "pr-12.md",
		"${each.value.sha}":      "a1",
		"${each.value.n}":        "12345678901234567890",
		"${each.value.ok}":       "true",
		"${each.value.tags.1}":   "y",
		"${each.value.meta}":     `{"a":1}`,
		"${each.value}":          `{"meta":{"a":1},"n":12345678901234567890,"ok":true,"sha":"a1","tags":["x","y"]}`,
		"$HOME ${date} ${other}": "$HOME ${date} ${other}",
	} {
		if got, err := substituteEach(in, item); err != nil || got != want {
			t.Errorf("substituteEach(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := substituteEach("${each.value.nope}", item); err == nil || !strings.Contains(err.Error(), `no field "nope"`) {
		t.Errorf("missing field: %v", err)
	}
}

func TestForEach_Instance(t *testing.T) {
	tmpl := Task{
		Name:         "review",
		Command:      []string{"review", "${each.key}"},
		Env:          []string{"SHA=${each.value.sha}"},
		Repo:         &Repo{URL: "https://example.com/r.git"},
		Path:         "/c/.cronicle/repos/r.git/s/review",
		CroniclePath: "/c",
		ForEach:      &ForEach{Source: "list"},
	}
	inst, err := tmpl.instance(ForEachItem{Key: "org/x", Value: json.RawMessage(`{"sha":"a1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Name != "review[org/x]" || inst.ForEach != nil || inst.forEachOf != "review" {
		t.Errorf("instance identity: %+v", inst)
	}
	if got := inst.fillEach(inst.Command[1]); got != "org/x" {
		t.Errorf("fillEach: %q", got)
	}
	if env := strings.Join(inst.eachEnv(inst.Env), "|"); env != `SHA=a1|EACH_KEY=org/x|EACH_VALUE={"sha":"a1"}` {
		t.Errorf("env: %s", env)
	}
	if got := inst.rerunLine([]string{"sh", "-c", "echo $EACH_KEY"}); got != `EACH_KEY=org/x EACH_VALUE='{"sha":"a1"}' sh -c 'echo $EACH_KEY'` {
		t.Errorf("rerun line must set the item's env: %s", got)
	}
	if inst.Path != "/c/.cronicle/repos/r.git/s/review/org_x" || fileSafeName(inst.Name) != "review-org_x" {
		t.Errorf("each item needs its own checkout, named safely: %s", inst.Path)
	}
	if tmpl.fillEach("${each.key}") != "${each.key}" || tmpl.rerunLine([]string{"ls"}) != "ls" {
		t.Error("a task that isn't an item must be left alone")
	}
	if _, err := tmpl.instance(ForEachItem{Key: "k", Value: json.RawMessage(`{}`)}); err == nil {
		t.Error("an item missing a field the task reads should fail when built")
	}
}

// TestForEach_ItemTextIsNotExpanded: an item's value goes in last, after
// ${date}-style tokens and $secret.NAME references are resolved, so text
// that looks like either reaches the task literally.
func TestForEach_ItemTextIsNotExpanded(t *testing.T) {
	cleanup := setupSecretStore(t, map[string]string{"TOKEN": "s3cret"})
	defer cleanup()
	captureLog(t)
	tmpl := Task{
		Name:    "show",
		Command: []string{"sh", "-c", `printf '%s|%s|%s' "$0" "$EACH_VALUE" "$V"`, "${each.value}"},
		Env:     []string{"V=${each.value}", "T=$secret.TOKEN"},
		Path:    t.TempDir(),
		ForEach: &ForEach{},
	}
	inst, err := tmpl.instance(ForEachItem{Key: "k", Value: json.RawMessage(`"$secret.TOKEN ${date}"`)})
	if err != nil {
		t.Fatal(err)
	}
	res := inst.Exec(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	want := "$secret.TOKEN ${date}|$secret.TOKEN ${date}|$secret.TOKEN ${date}"
	if res.Stdout != want {
		t.Errorf("item text was expanded:\n got %q\nwant %q (err %v)", res.Stdout, want, res.Error)
	}
}

// ---- walker ----------------------------------------------------------------

// fakeRun records the order nodes ran in and fails the names in fail.
type fakeRun struct {
	mu      sync.Mutex
	order   []string
	fail    map[string]bool
	active  int
	maxSeen int
	delay   time.Duration
}

func (f *fakeRun) run(name string) error {
	f.mu.Lock()
	f.active++
	f.maxSeen = max(f.maxSeen, f.active)
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active--
	f.order = append(f.order, name)
	if f.fail[name] {
		return fmt.Errorf("%s failed", name)
	}
	return nil
}

func (f *fakeRun) index(name string) int {
	for i, n := range f.order {
		if n == name {
			return i
		}
	}
	return -1
}

func TestRunDAG_ForEachPairsAndFansIn(t *testing.T) {
	f := &fakeRun{fail: map[string]bool{"review[b]": true}}
	var skipped []string
	var done []string
	err := runDAG(dagSpec{
		deps: map[string][]string{
			"list":    nil,
			"review":  {"list"},
			"report":  {"review"},
			"summary": {"report"},
		},
		continueOnFailure: map[string]bool{"summary": true},
		forEach: map[string]dagForEach{
			"review": {source: "list"},
			"report": {source: "list"},
		},
		order: []string{"list", "review", "report", "summary"},
	}, dagHooks{
		run:         f.run,
		expand:      func(string) ([]string, error) { return []string{"a", "b", "c"}, nil },
		onDepFailed: func(node, _ string) { skipped = append(skipped, node) },
		onForEachDone: func(name string, items, failed int) {
			done = append(done, fmt.Sprintf("%s:%d/%d", name, failed, items))
		},
	})
	if err == nil || !strings.Contains(err.Error(), "review[b] failed") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(skipped, ",") != "report[b]" {
		t.Errorf("only report[b] should be skipped, got %v", skipped)
	}
	for _, k := range []string{"a", "c"} {
		if f.index("report["+k+"]") < f.index("review["+k+"]") {
			t.Errorf("report[%s] ran before review[%s]: %v", k, k, f.order)
		}
	}
	if last := f.order[len(f.order)-1]; last != "summary" {
		t.Errorf("summary must run last, after every item: %v", f.order)
	}
	if strings.Join(done, " ") != "review:1/3 report:1/3" {
		t.Errorf("for_each completions: %v", done)
	}
}

func TestRunDAG_ForEachEdgeCases(t *testing.T) {
	// Empty collection: a successful no-op that releases its dependents.
	f := &fakeRun{}
	err := runDAG(dagSpec{
		deps:    map[string][]string{"list": nil, "each": {"list"}, "after": {"each"}},
		forEach: map[string]dagForEach{"each": {source: "list"}},
	}, dagHooks{run: f.run, expand: func(string) ([]string, error) { return nil, nil }})
	if err != nil || strings.Join(f.order, ",") != "list,after" {
		t.Errorf("empty: err=%v order=%v", err, f.order)
	}

	// Source fails: the for_each task (and so its fan-in) is skipped.
	f = &fakeRun{fail: map[string]bool{"list": true}}
	var skipped []string
	_ = runDAG(dagSpec{
		deps:    map[string][]string{"list": nil, "each": {"list"}, "after": {"each"}},
		forEach: map[string]dagForEach{"each": {source: "list"}},
	}, dagHooks{run: f.run, expand: func(string) ([]string, error) {
		t.Error("expand called after the source failed")
		return nil, nil
	}, onDepFailed: func(node, _ string) { skipped = append(skipped, node) }})
	if strings.Join(skipped, ",") != "each,after" {
		t.Errorf("source failure should skip each and after: %v", skipped)
	}

	// Expansion error fails the node.
	f = &fakeRun{}
	err = runDAG(dagSpec{
		deps:    map[string][]string{"list": nil, "each": {"list"}},
		forEach: map[string]dagForEach{"each": {source: "list"}},
	}, dagHooks{run: f.run, expand: func(string) ([]string, error) { return nil, fmt.Errorf("bad json") }})
	if err == nil || err.Error() != "bad json" {
		t.Errorf("expand error: %v", err)
	}

	// max_parallel bounds concurrent items.
	f = &fakeRun{delay: 20 * time.Millisecond}
	err = runDAG(dagSpec{
		deps:    map[string][]string{"each": nil},
		forEach: map[string]dagForEach{"each": {maxParallel: 2}},
	}, dagHooks{run: f.run, expand: func(string) ([]string, error) { return []string{"1", "2", "3", "4", "5"}, nil }})
	if err != nil || len(f.order) != 5 || f.maxSeen != 2 {
		t.Errorf("max_parallel=2: err=%v ran=%d maxConcurrent=%d", err, len(f.order), f.maxSeen)
	}

	// A static collection expands only once its deps are done.
	f = &fakeRun{}
	var expandedAfter []string
	_ = runDAG(dagSpec{
		deps:    map[string][]string{"first": nil, "each": {"first"}},
		forEach: map[string]dagForEach{"each": {}},
	}, dagHooks{run: f.run, expand: func(string) ([]string, error) {
		expandedAfter = append([]string(nil), f.order...)
		return []string{"x"}, nil
	}})
	if strings.Join(expandedAfter, ",") != "first" || strings.Join(f.order, ",") != "first,each[x]" {
		t.Errorf("static for_each expanded before its deps: expanded after %v, ran %v", expandedAfter, f.order)
	}

	// A for_each over a different source waits for every item, not pairs.
	f = &fakeRun{}
	_ = runDAG(dagSpec{
		deps: map[string][]string{"l1": nil, "l2": nil, "a": {"l1"}, "b": {"l2", "a"}},
		forEach: map[string]dagForEach{
			"a": {source: "l1"},
			"b": {source: "l2"},
		},
	}, dagHooks{run: f.run, expand: func(name string) ([]string, error) { return []string{"x", "y"}, nil }})
	for _, b := range []string{"b[x]", "b[y]"} {
		if f.index(b) < f.index("a[x]") || f.index(b) < f.index("a[y]") {
			t.Errorf("%s must wait for all of a: %v", b, f.order)
		}
	}
}

// ---- end to end ------------------------------------------------------------

// captureLog routes slog to a JSON buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func events(buf *bytes.Buffer, entryType string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["entry_type"] == entryType {
			out = append(out, m)
		}
	}
	return out
}

const prsHCL = `
schedule "prs" {
  task "list" {
    command = ["echo", "[{\"number\":12,\"sha\":\"a1\"},{\"number\":15,\"sha\":\"b2\"}]"]
  }
  task "review" {
    for_each = { for pr in task.list.output : pr.number => pr }
    command  = ["sh", "-c", "echo review $EACH_KEY $0; test $EACH_KEY != 15", each.value.sha]
  }
  task "report" {
    for_each = { for pr in task.list.output : pr.number => pr }
    depends  = ["review"]
    command  = ["echo", "report", each.key]
  }
  task "summary" {
    depends             = ["report"]
    continue_on_failure = true
    command             = ["echo", "done"]
  }
}`

func TestExecuteTasks_ForEach(t *testing.T) {
	buf := captureLog(t)
	conf, err := parseConfig(t, prsHCL)
	if err != nil {
		t.Fatal(err)
	}
	sched := conf.Schedules[0]
	sched.RunID = "R1"
	if err := sched.ExecuteTasks(); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("review[15] fails, so the run should: %v", err)
	}

	ran := map[string]string{}
	for _, e := range events(buf, "shell_run") {
		ran[e["task"].(string)] = strings.TrimSpace(e["stdout"].(string))
	}
	want := map[string]string{
		"review[12]": "review 12 a1",
		"review[15]": "review 15 b2",
		"report[12]": "report 12",
		"summary":    "done",
	}
	for name, out := range want {
		if ran[name] != out {
			t.Errorf("%s printed %q, want %q", name, ran[name], out)
		}
	}
	if _, ok := ran["report[15]"]; ok {
		t.Error("report[15] ran though review[15] failed")
	}
	exp := events(buf, "task_expanded")
	if len(exp) != 2 || exp[0]["task"] != "review" || exp[0]["count"] != float64(2) {
		t.Fatalf("task_expanded: %v", exp)
	}
	var items []ForEachItem
	if err := json.Unmarshal([]byte(exp[0]["for_each_items"].(string)), &items); err != nil || len(items) != 2 || items[1].Key != "15" {
		t.Errorf("recorded items: %v %v", items, err)
	}
	start := events(buf, "schedule_start")[0]
	if fmt.Sprint(start["for_each"]) != "[ * * ]" {
		t.Errorf("schedule_start for_each attr: %v", start["for_each"])
	}
}

// TestExecuteTasks_ForEachRetryPayload: a retried run carries the recorded
// items (and the keys to skip) and has no source task; it reruns just the
// remaining items, still pairing report with review.
func TestExecuteTasks_ForEachRetryPayload(t *testing.T) {
	buf := captureLog(t)
	conf, err := parseConfig(t, prsHCL)
	if err != nil {
		t.Fatal(err)
	}
	orig := conf.Schedules[0]
	recorded := []ForEachItem{{Key: "12", Value: json.RawMessage(`{"number":12,"sha":"a1"}`)}, {Key: "15", Value: json.RawMessage(`{"number":15,"sha":"c3"}`)}}
	var retry Schedule
	b, _ := json.Marshal(orig)
	if err := json.Unmarshal(b, &retry); err != nil {
		t.Fatal(err)
	}
	var kept []Task
	for _, tk := range retry.Tasks {
		if tk.Name == "list" {
			continue
		}
		if tk.ForEach != nil {
			tk.ForEach.Items, tk.ForEach.Skip = recorded, []string{"12"}
		}
		kept = append(kept, tk)
	}
	retry.Tasks = kept
	retry.RunID = "R2"
	_ = retry.ExecuteTasks()

	var ran []string
	for _, e := range events(buf, "shell_run") {
		ran = append(ran, e["task"].(string))
	}
	if strings.Join(ran, ",") != "review[15],summary" {
		t.Errorf("retry ran %v; want review[15] (fails again), then summary", ran)
	}
	if e := events(buf, "task_expanded"); len(e) == 0 || e[0]["recorded"] != true {
		t.Errorf("expansion should say it used recorded items: %v", e)
	}
}

func TestRenderForEach(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()
	render := func(attrs ...any) string {
		rec := slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0)
		rec.Add(attrs...)
		var b bytes.Buffer
		if err := renderForEach(&b, rec); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(b.String())
	}
	for got, want := range map[string]string{
		render("entry_type", "task_expanded", "task", "review", "source", "list", "count", 3, "keys", []string{"12", "15", "20"}): "⋔ review  3 items from list: 12, 15, 20",
		render("entry_type", "task_expanded", "task", "review", "source", "list", "count", 0, "keys", []string{}):                 "⋔ review  no items from list, nothing to run",
		render("entry_type", "for_each_complete", "task", "review", "items", 3, "failed", 1):                                      "⋔ review  done · 3 items · 1 failed",
		render("entry_type", "task_error", "task", "review[15]", "error", "each.value.sha: no field \"sha\"\nmore"):               "■ review[15] failed  each.value.sha: no field \"sha\"",
	} {
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	}

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "schedule started", 0)
	rec.AddAttrs(
		slog.String("entry_type", "schedule_start"), slog.String("schedule", "prs"),
		slog.Any("tasks", []string{"list", "review"}),
		slog.Any("depends", []string{"", "list"}),
		slog.Any("for_each", []string{"", "*"}),
	)
	var b bytes.Buffer
	_ = renderScheduleStart(&b, rec)
	if !strings.Contains(b.String(), "  list\n  └─ review[*]\n") {
		t.Errorf("DAG should draw for_each tasks as name[*]:\n%s", b.String())
	}
}

// TestExecuteTasks_ForEachRepoCheckout: each.* in a repo block picks what
// each item checks out — here, one commit per item — into its own
// working tree, so items running side by side never share a checkout.
func TestExecuteTasks_ForEachRepoCheckout(t *testing.T) {
	buf := captureLog(t)
	src := scratchRepo(t)
	r, err := git.PlainOpen(src)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := r.Head()
	second := head.Hash().String()
	c, _ := r.CommitObject(head.Hash())
	first := c.ParentHashes[0].String()

	conf, err := parseConfig(t, fmt.Sprintf(`
schedule "s" {
  task "list" {
    command = ["echo", "{\"one\": {\"sha\": \"%s\"}, \"two\": {\"sha\": \"%s\"}}"]
  }
  task "show" {
    for_each = task.list.output
    repo {
      url    = %q
      commit = each.value.sha
    }
    command = ["cat", "hello.txt"]
  }
}`, first, second, src))
	if err != nil {
		t.Fatal(err)
	}
	sched := conf.Schedules[0]
	sched.RunID = "R1"
	if err := sched.ExecuteTasks(); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	paths := map[string]bool{}
	for _, e := range events(buf, "shell_run") {
		got[e["task"].(string)] = strings.TrimSpace(e["stdout"].(string))
		paths[e["path"].(string)] = true
	}
	if got["show[one]"] != "first version" || got["show[two]"] != "second version" {
		t.Errorf("each item should check out its own commit: %v", got)
	}
	if len(paths) != 3 {
		t.Errorf("want a separate working tree per item: %v", paths)
	}
	for _, e := range events(buf, "shell_run") {
		if e["task"] != "show[one]" {
			continue
		}
		lines := fmt.Sprint(e["checkout"])
		for _, want := range []string{"clone " + src, "checkout commit " + first[:7], "at " + first[:7] + " Add hello.txt"} {
			if !strings.Contains(lines, want) {
				t.Errorf("show[one] checkout lines missing %q: %s", want, lines)
			}
		}
	}
	if steps := events(buf, "task_checkout"); len(steps) == 0 || steps[0]["task"] == nil {
		t.Errorf("git steps should still be logged, tagged with the task: %v", steps)
	}
	if sched.TaskMap()["show"].Repo.Commit != "${each.value.sha}" {
		t.Error("an item changed the for_each task's shared repo block")
	}
}

// TestAgentWorkspaceNote: an agent is told where its tools run — for a
// for_each item with a repo, its own checkout and the commit on it — and
// the operator sees the same in the header.
func TestAgentWorkspaceNote(t *testing.T) {
	plain := Task{Path: "/c", CroniclePath: "/c", ScratchDir: "/c/.cronicle/scratch/s/R1"}
	if got := plain.workspaceNote(); got != "## Workspace\n\nYour working directory is /c. The bash tool starts there. text_editor "+
		"and git take paths relative to it (`src/app.py`) or absolute paths under it (`/c/src/app.py`). "+
		"Files to hand to other tasks in this run go in /c/.cronicle/scratch/s/R1, which already exists." {
		t.Errorf("plain note: %q", got)
	}
	if plain.workspaceRow() != "" {
		t.Error("a task in the project dir needs no workspace row")
	}

	tmpl := Task{
		Name: "review", CroniclePath: "/c", Path: "/c/.cronicle/repos/org/app.git/prs/review",
		Repo:    &Repo{URL: "https://x:tok@github.com/org/app.git", Commit: "${each.value.sha}"},
		ForEach: &ForEach{Source: "list"},
	}
	inst, _ := tmpl.instance(ForEachItem{Key: "12", Value: json.RawMessage(`{"sha":"a1b2c3d4e5"}`)})
	inst.Repo = inst.eachRepo(inst.Repo) // what Execute does before checking out
	note := inst.workspaceNote()
	for _, want := range []string{
		"/c/.cronicle/repos/org/app.git/prs/review/12",
		"It is a git checkout at commit a1b2c3d4e5, cloned from https://github.com/org/app.git.",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "tok") {
		t.Error("credentials leaked into the system prompt")
	}
	if row := inst.workspaceRow(); !strings.HasSuffix(row, "review/12 @ commit a1b2c3d4e5") {
		t.Errorf("workspace row: %q", row)
	}
}

// TestAgentBashOutputInGutter: an agent's bash tool writes through the
// frame's gutter, so its output stays inside the block.
func TestAgentBashOutputInGutter(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()
	var b bytes.Buffer
	gw := newGutterWriter(&b, agentAccent)
	for _, tool := range buildAgentTools([]string{"bash"}, t.TempDir(), nil, nil, "", gw) {
		if tool.Name() == "bash" {
			tool.Execute(context.Background(), json.RawMessage(`{"command":"echo one; echo two"}`))
		}
	}
	gw.EndLine()
	if !strings.Contains(b.String(), "│ one\n│ two\n") {
		t.Errorf("bash output left the gutter:\n%q", b.String())
	}
}

// TestCheckoutFailure_IsATaskError: a checkout that fails ends the task
// with a task_error, so it gets a terminal state and a line in the output.
func TestCheckoutFailure_IsATaskError(t *testing.T) {
	buf := captureLog(t)
	conf, err := parseConfig(t, fmt.Sprintf(`
schedule "s" {
  task "show" {
    repo {
      url    = %q
      commit = "0000000000000000000000000000000000000001"
    }
    command = ["cat", "hello.txt"]
  }
}`, scratchRepo(t)))
	if err != nil {
		t.Fatal(err)
	}
	sched := conf.Schedules[0]
	if err := sched.ExecuteTasks(); err == nil || !strings.Contains(err.Error(), "checkout:") {
		t.Fatalf("want a checkout error, got %v", err)
	}
	if e := events(buf, "task_error"); len(e) != 1 || e[0]["task"] != "show" {
		t.Errorf("want one task_error for show: %v", e)
	}
}

func TestFrameHeader_GitSection(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()
	var b bytes.Buffer
	WriteShellRunHeader(&b, "s", "show", []string{"cat", "hello.txt"}, []string{"clone https://x/r.git", "checkout commit 1a2b3c4"})
	want := "╭─ show  shell · s\n│ $ cat hello.txt\n├─ git ──────────────────\n│ clone https://x/r.git\n│ checkout commit 1a2b3c4\n├────────────────────────\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestAgentWorkspaceNote_LocalRepoURL(t *testing.T) {
	task := Task{Name: "r", CroniclePath: "/c", Path: "/c/.cronicle/repos/src/app/s/r",
		Repo: &Repo{URL: "/src/app", Commit: "abc1234"}}
	if note := task.workspaceNote(); strings.Contains(note, "/src/app,") || strings.Contains(note, "cloned from") {
		t.Errorf("a local repo path must not be offered as a place to look:\n%s", note)
	}
}

func TestGitTool_Show(t *testing.T) {
	g := &GitTool{Workspace: scratchRepo(t)}
	run := func(input string) (string, bool) {
		return g.Execute(context.Background(), json.RawMessage(input))
	}
	if out, isErr := run(`{"command":"show","args":{"ref":"HEAD~1","path":"hello.txt"}}`); isErr || out != "first version\n" {
		t.Errorf("file at a ref: %q %v", out, isErr)
	}
	if out, isErr := run(`{"command":"show","args":{"path":"hello.txt"}}`); isErr || out != "second version\n" {
		t.Errorf("file at HEAD (not the dirty worktree): %q %v", out, isErr)
	}
	out, isErr := run(`{"command":"show"}`)
	if isErr || !strings.Contains(out, "Update hello.txt") || !strings.Contains(out, "hello.txt | +1 -1") {
		t.Errorf("commit details: %q %v", out, isErr)
	}
	if _, isErr := run(`{"command":"show","args":{"path":"nope.txt"}}`); !isErr {
		t.Error("a missing file should be an error")
	}
}

func TestGitCallSummary(t *testing.T) {
	for want, call := range map[string]map[string]any{
		"log -n 1":                          {"command": "log", "args": map[string]any{"limit": 1.0}},
		"log":                               {"command": "log"},
		"diff origin/main..HEAD -- calc.py": {"command": "diff", "args": map[string]any{"from": "origin/main", "to": "HEAD", "path": "calc.py"}},
		"show origin/main:calc.py":          {"command": "show", "args": map[string]any{"ref": "origin/main", "path": "calc.py"}},
		"show HEAD":                         {"command": "show"},
		`commit -m "fix it"`:                {"command": "commit", "args": map[string]any{"message": "fix it"}},
		"push origin main":                  {"command": "push", "args": map[string]any{"remote": "origin", "branch": "main"}},
	} {
		if got := gitCallSummary(call["command"].(string), call["args"]); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// TestEditorOutsideWorkspaceError: a wrong path gets an error that says
// where the workspace is and how the path should look, so the agent can
// fix it in one step.
func TestEditorOutsideWorkspaceError(t *testing.T) {
	ws, scratch := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "calc.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := &TextEditorTool{Workspace: ws, AllowedRoots: []string{scratch}}
	out, isErr := ed.Execute(context.Background(), json.RawMessage(`{"command":"view","path":"/elsewhere/app/calc.py"}`))
	wsAbs, _ := filepath.Abs(ws)
	for _, want := range []string{"outside the workspace", "The workspace is " + wsAbs, filepath.Join(wsAbs, "calc.py"), "(calc.py)", "Also allowed: " + scratch} {
		if !isErr || !strings.Contains(out, want) {
			t.Errorf("error missing %q: %s", want, out)
		}
	}
	out, _ = ed.Execute(context.Background(), json.RawMessage(`{"command":"view","path":"."}`))
	if !strings.HasPrefix(out, "Directory . ("+wsAbs+"):") {
		t.Errorf("a listing should name the directory absolutely too: %q", out)
	}
}
