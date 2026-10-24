package cronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// lastSuccessfulRunStart returns the start time of the most recent
// SUCCEEDED run of the named schedule, excluding the in-flight run.
// Best-effort: ok=false when there's no state backend, no prior success,
// or the query errors — callers treat that as "no prior run".
//
// The store is passed in explicitly so the producer resolves ${last_run}
// against its authoritative state backend at enqueue time (enqueueAdapter)
// — never the worker, whose local store is a non-authoritative in-memory
// projection. See Schedule.LastRunResolved.
func lastSuccessfulRunStart(st state.Backend, schedule, currentRunID string) (time.Time, bool) {
	if st == nil {
		return time.Time{}, false
	}
	runs, err := st.ListRuns(state.ListFilter{Schedule: schedule, Status: "succeeded", Limit: 1})
	if err != nil {
		slog.Warn("last-run lookup failed; ${last_run} will be empty",
			"schedule", schedule, "error", err.Error())
		return time.Time{}, false
	}
	for _, r := range runs {
		if r.RunID == currentRunID || r.StartedAt.IsZero() {
			continue
		}
		return r.StartedAt, true
	}
	return time.Time{}, false
}

// resolveLastRun stamps every task's LastRun with the schedule's previous
// successful run start, read from the authoritative state store st.
//
// Call this at DISPATCH time — the producer's enqueueAdapter, or the
// in-process ConsumeSchedule / ExecTasks paths — so the value is baked
// before ExecuteTasks runs. ExecuteTasks must NOT resolve ${last_run}
// itself: it also runs on distributed workers, where StateStore() is a
// non-authoritative in-memory projection with no run history. Resolving
// only at dispatch keeps the producer the single source of truth.
//
// Best-effort: a miss (no prior run, no/again-unavailable store) leaves
// LastRun zero → ${last_run} empty → "full backfill".
func resolveLastRun(st state.Backend, sch *Schedule) {
	if lastRun, ok := lastSuccessfulRunStart(st, sch.Name, sch.RunID); ok {
		for i := range sch.Tasks {
			sch.Tasks[i].LastRun = lastRun
		}
	}
}

// ExecuteTasks executes all tasks in dependency order, running independent tasks concurrently.
func (schedule Schedule) ExecuteTasks() error {
	var now time.Time
	if (schedule.Now == time.Time{}) {
		now = time.Now().In(time.Local)
	} else {
		now = schedule.Now
	}

	taskMap := schedule.TaskMap()

	deps := make(map[string][]string)
	// shownDeps is deps as the DAG is drawn: a for_each task's automatic
	// wait on its source is left out when its own depends already lead there.
	shownDeps := make(map[string][]string)
	taskNames := make([]string, 0, len(schedule.Tasks))
	forEach := make(map[string]dagForEach)
	sources := make(map[string]bool)
	forEachAttr := make([]string, 0, len(schedule.Tasks))
	for _, task := range schedule.Tasks {
		deps[task.Name] = task.Depends
		shownDeps[task.Name] = task.Depends
		taskNames = append(taskNames, task.Name)
		if fe := task.ForEach; fe != nil {
			// A for_each task waits on its source even when depends
			// doesn't say so. Recorded items (a retry) need no source.
			source := fe.Source
			if fe.Items == nil && source != "" && !contains(deps[task.Name], source) {
				deps[task.Name] = append([]string{source}, deps[task.Name]...)
				if len(task.Depends) == 0 {
					shownDeps[task.Name] = []string{source}
				}
			}
			forEach[task.Name] = dagForEach{source: source, maxParallel: fe.MaxParallel}
			if source != "" {
				sources[source] = true
			}
			forEachAttr = append(forEachAttr, "*")
		} else {
			forEachAttr = append(forEachAttr, "")
		}
	}

	// Ensure tasks know their parent run id so per-task events carry it.
	// Producers (ProduceSchedule, listener trigger handlers, ExecTasks) set
	// schedule.RunID before reaching here; if a caller didn't, mint one
	// now so the projection still gets a coherent grouping rather than
	// silently dropping events.
	if schedule.RunID == "" {
		schedule.RunID = newRunID()
	}
	for i := range schedule.Tasks {
		schedule.Tasks[i].RunID = schedule.RunID
		schedule.Tasks[i].RunCtx = schedule.RunCtx
	}
	taskMap = schedule.TaskMap()

	// Schedule-scoped scratch dir: a single dir per schedule run, shared by
	// every task within it via task.ScratchDir (substituted into prompts as
	// ${scratch}). The cronicle-native pattern for cross-task context: an
	// upstream task writes a file, a downstream task reads it. Survives the
	// run so transcripts and post-hoc audits can reference produced
	// artifacts. Created best-effort — if mkdir fails, tasks just see an
	// empty ScratchDir and ${scratch} substitution is a no-op.
	scratchDir := schedule.scratchDirFor(now)
	if scratchDir != "" {
		_ = os.MkdirAll(scratchDir, 0o755)
		for name, t := range taskMap {
			t.ScratchDir = scratchDir
			taskMap[name] = t
		}
	}

	// ${last_run} / ${last_run_epoch} come straight from task.LastRun,
	// which the DISPATCHER already resolved (resolveLastRun) — the producer
	// at enqueue, or the in-process ConsumeSchedule / ExecTasks paths.
	// ExecuteTasks itself never reads state for this: it also runs on
	// distributed workers, whose StateStore() is a non-authoritative
	// in-memory projection with no run history. Empty → ${last_run} empty
	// → "no prior run / full backfill".

	startedAt := time.Now()
	startAttrs := []any{
		"entry_type", "schedule_start",
		"run_id", schedule.RunID,
		"schedule", schedule.Name,
		"source", schedule.Source,
		"clock", now.Format(time.Kitchen),
		"date", now.Format(time.RFC850),
		"tasks", taskNames,
		"depends", dependsAttr(taskNames, shownDeps),
		"dag", dagString(taskNames, shownDeps),
	}
	if len(forEach) > 0 {
		startAttrs = append(startAttrs, "for_each", forEachAttr)
	}
	if scratchDir != "" {
		startAttrs = append(startAttrs, "scratch", scratchDir)
	}
	slog.Info("schedule started", startAttrs...)

	continueOnFailure := make(map[string]bool)
	for _, task := range schedule.Tasks {
		if task.ContinueOnFailure {
			continueOnFailure[task.Name] = true
		}
	}

	// Tasks are looked up by name as the walk reaches them; for_each items
	// are added as their collections expand. outputs keeps the stdout of
	// every task a for_each reads.
	var mu sync.Mutex
	outputs := make(map[string]string)
	fillErrs := make(map[string]error)
	lookup := func(name string) (Task, error) {
		mu.Lock()
		defer mu.Unlock()
		return taskMap[name], fillErrs[name]
	}

	run := func(name string) error {
		task, fillErr := lookup(name)
		// Run-pause gate: a POST /v1/runs/{id}/pause may land mid-walk.
		// Block here before launching the task body. Resume (POST
		// /v1/runs/{id}/unpause) clears the flag; the poll loop picks
		// up on its next tick. Cancel of the whole run (via ctx) takes
		// precedence — a paused run that's then canceled exits the
		// loop immediately rather than waiting for unpause.
		awaitRunPauseClear(schedule.RunCtx, schedule.RunID, name, schedule.Name)
		// Per-task cancel gate: a POST /v1/runs/{id}/tasks/{task}/cancel
		// landed against this run before the walker reached this node.
		// The projection already shows status='canceled' for this task
		// (sticky); the walker emits the matching event and returns nil
		// so downstream cascade entries are also walked-and-skipped.
		// Cancel takes precedence over skip — if both flags are set,
		// the cancel record wins. An item is canceled or skipped along
		// with its for_each task.
		if taskCanceledInRun(schedule.RunID, name) || (task.forEachOf != "" && taskCanceledInRun(schedule.RunID, task.forEachOf)) {
			slog.Info("task canceled",
				"entry_type", "task_canceled",
				"run_id", schedule.RunID,
				"schedule", schedule.Name,
				"task", name,
				"reason", "canceled via API before launch",
			)
			return nil
		}
		// Per-task skip gate. Operators set the flag via
		// POST /v1/schedules/{name}/tasks/{task}/skip; the DAG walker
		// records a task_skipped event and treats the node as a
		// vacuous success so dependents still run. Failing open on
		// store errors mirrors the schedule-pause gate.
		skipped, why := taskIsSkipped(schedule.Name, name)
		if !skipped && task.forEachOf != "" {
			skipped, why = taskIsSkipped(schedule.Name, task.forEachOf)
		}
		if skipped {
			slog.Info("task skipped",
				"entry_type", "task_skipped",
				"run_id", schedule.RunID,
				"schedule", schedule.Name,
				"task", name,
				"reason", why,
			)
			return nil
		}
		if fillErr != nil {
			logTaskError(schedule, name, fillErr)
			return fmt.Errorf("%s: %w", name, fillErr)
		}
		res, err := task.Execute(now)
		if err == nil && sources[name] {
			mu.Lock()
			outputs[name] = res.Stdout
			mu.Unlock()
		}
		return err
	}

	expand := func(name string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		tmpl := taskMap[name]
		fe := tmpl.ForEach
		var items []ForEachItem
		_, inRun := taskMap[fe.Source]
		_, succeeded := outputs[fe.Source]
		err := fmt.Errorf("for_each source %s didn't succeed, so there are no items to run", fe.Source)
		if fe.Items != nil || fe.Source == "" || succeeded || !inRun {
			items, err = fe.expand(outputs)
		}
		if err != nil {
			logTaskError(schedule, name, err)
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, item := range items {
			inst, fillErr := tmpl.instance(item)
			taskMap[inst.Name] = inst
			if fillErr != nil {
				fillErrs[inst.Name] = fillErr
			}
		}
		recorded, _ := json.Marshal(items)
		attrs := []any{
			"entry_type", "task_expanded",
			"run_id", schedule.RunID,
			"schedule", schedule.Name,
			"task", name,
			"source", fe.Source,
			"count", len(items),
			"keys", forEachKeys(items),
			// The full collection, so a retry of this run can reuse it.
			"for_each_items", string(recorded),
		}
		if fe.Items != nil {
			attrs = append(attrs, "recorded", true)
		}
		slog.Info("task expanded", attrs...)
		return forEachKeys(items), nil
	}

	walkErr := runDAG(dagSpec{deps: deps, continueOnFailure: continueOnFailure, forEach: forEach, order: taskNames}, dagHooks{
		run:    run,
		expand: expand,
		onDepFailed: func(node, failedDep string) {
			slog.Warn("task skipped: upstream dependency failed",
				"entry_type", "task_dep_failed",
				"run_id", schedule.RunID,
				"schedule", schedule.Name,
				"task", node,
				"failed_dependency", failedDep,
			)
		},
		onForEachDone: func(name string, items, failed int) {
			level := slog.LevelInfo
			if failed > 0 {
				level = slog.LevelWarn
			}
			slog.Log(context.Background(), level, "for_each complete",
				"entry_type", "for_each_complete",
				"run_id", schedule.RunID,
				"schedule", schedule.Name,
				"task", name,
				"items", items,
				"failed", failed,
				"success", failed == 0,
			)
		},
	})
	durationMs := time.Since(startedAt).Milliseconds()

	if walkErr != nil {
		slog.Error("schedule failed",
			"entry_type", "schedule_complete",
			"run_id", schedule.RunID,
			"schedule", schedule.Name,
			"task_count", len(schedule.Tasks),
			"duration_ms", durationMs,
			"success", false,
			"error", walkErr.Error(),
		)
	} else {
		slog.Info("schedule complete",
			"entry_type", "schedule_complete",
			"run_id", schedule.RunID,
			"schedule", schedule.Name,
			"task_count", len(schedule.Tasks),
			"duration_ms", durationMs,
			"success", true,
		)
	}
	return walkErr
}

// walkDAG executes fn for each node in topological order, running independent
// nodes concurrently. When fn returns an error for a node, all transitive
// dependents are skipped (marked failed without calling fn) unless
// continueOnFailure[dependent] is true. onDepFailed is called for every
// skipped dependent with the name of the first failed upstream dependency.
func walkDAG(deps map[string][]string, fn func(string) error, continueOnFailure map[string]bool, onDepFailed func(node, failedDep string)) error {
	return runDAG(dagSpec{deps: deps, continueOnFailure: continueOnFailure},
		dagHooks{run: fn, onDepFailed: onDepFailed})
}

// dagSpec is a DAG's static shape. forEach marks the nodes that expand
// into one node per item once their source finishes (see foreach.go).
type dagSpec struct {
	deps              map[string][]string
	continueOnFailure map[string]bool
	forEach           map[string]dagForEach
	// order is declaration order, so ties launch (and log) predictably.
	order []string
}

type dagForEach struct {
	source      string // "" when the items are known up front
	maxParallel int
}

// dagHooks are runDAG's callbacks. All but run are called from the
// walker's own goroutine, one at a time.
type dagHooks struct {
	run         func(name string) error
	onDepFailed func(node, failedDep string)
	// expand returns a for_each node's item keys; an error fails the node.
	expand func(name string) ([]string, error)
	// onForEachDone reports a for_each node finishing: how many items it
	// had and how many failed or were skipped.
	onForEachDone func(name string, items, failed int)
}

type dagNode struct {
	name              string
	rank              int // declaration order; an item shares its for_each task's
	deps              []string
	dependents        []*dagNode
	pending           int // unfinished deps
	continueOnFailure bool
	forEach           *dagForEach // set until expanded
	items             []*dagNode  // set once expanded
	limit             *dagLimit
	readied           bool
	finished, failed  bool
}

// dagLimit is a for_each node's max_parallel, shared by its items.
type dagLimit struct {
	max, active int
	queue       []*dagNode
}

type dagWalk struct {
	spec     dagSpec
	hooks    dagHooks
	nodes    map[string]*dagNode
	done     chan dagResult
	running  int
	firstErr error
}

type dagResult struct {
	node *dagNode
	err  error
}

// runDAG walks the DAG. Nodes whose deps are done run concurrently; a
// for_each node, once its source is done, is replaced in the walk by its
// items and finishes when they all have.
func runDAG(spec dagSpec, hooks dagHooks) error {
	w := &dagWalk{spec: spec, hooks: hooks, nodes: make(map[string]*dagNode, len(spec.deps)), done: make(chan dagResult)}
	names := make([]string, 0, len(spec.deps))
	for name := range spec.deps {
		names = append(names, name)
	}
	rank := make(map[string]int, len(spec.order))
	for i, name := range spec.order {
		rank[name] = i + 1
	}
	// Order by depth in the DAG, then declaration: upstream work first, so
	// a for_each that another pairs with expands (and says so) before it.
	// Declaration alone can't do this — agent blocks are folded in after
	// task blocks.
	depth := dagDepths(spec.deps)
	sort.Slice(names, func(i, j int) bool {
		if di, dj := depth[names[i]], depth[names[j]]; di != dj {
			return di < dj
		}
		ri, rj := rank[names[i]], rank[names[j]]
		if ri != rj && ri > 0 && rj > 0 {
			return ri < rj
		}
		return names[i] < names[j]
	})
	for i, name := range names {
		rank[name] = i + 1
	}

	for _, name := range names {
		n := &dagNode{name: name, rank: rank[name], continueOnFailure: spec.continueOnFailure[name]}
		if fe, ok := spec.forEach[name]; ok {
			fe := fe
			n.forEach = &fe
			// Until it expands, a for_each node waits only on its source;
			// its other deps are waited on by each item, so items can pair
			// with another for_each's. A static collection has nothing to
			// pair with, so it waits on all its deps and expands (and says
			// so) when its turn comes rather than at the start of the run.
			if _, known := spec.deps[fe.source]; known {
				n.deps = []string{fe.source}
			} else if fe.source == "" {
				n.deps = spec.deps[name]
			}
		} else {
			n.deps = spec.deps[name]
		}
		w.nodes[name] = n
	}
	for _, name := range names {
		n := w.nodes[name]
		for _, dep := range n.deps {
			n.pending++ // an unknown dep never finishes: reported below
			if d, ok := w.nodes[dep]; ok {
				d.dependents = append(d.dependents, n)
			}
		}
	}
	var ready []*dagNode
	for _, name := range names {
		if w.nodes[name].pending == 0 {
			ready = append(ready, w.nodes[name])
		}
	}
	w.makeReady(ready)

	for w.running > 0 {
		r := <-w.done
		w.running--
		if r.err != nil && w.firstErr == nil {
			w.firstErr = r.err
		}
		if l := r.node.limit; l != nil {
			l.active--
			if len(l.queue) > 0 {
				next := l.queue[0]
				l.queue = l.queue[1:]
				w.launch(next)
			}
		}
		w.finish(r.node, r.err != nil)
	}

	// Cycle detection: any node still unfinished after the walk depends on
	// a node that never ran — only possible when those nodes form a
	// dependency cycle (or depend on an unknown task). Without this check,
	// a cyclic config would silently produce a zero-task no-op and return
	// nil. A cycle is a config-level fault, so it's surfaced ahead of any
	// task-level firstErr.
	var unreached []string
	for _, name := range names {
		if !w.nodes[name].finished {
			unreached = append(unreached, name)
		}
	}
	if len(unreached) > 0 {
		return fmt.Errorf("dependency cycle detected — tasks never executed: %s",
			strings.Join(unreached, ", "))
	}
	return w.firstErr
}

// makeReady handles nodes whose deps have all finished. for_each nodes go
// first: one may pair its items with another's (zip), so that one must be
// expanded before.
func (w *dagWalk) makeReady(nodes []*dagNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		fi, fj := nodes[i].forEach != nil, nodes[j].forEach != nil
		if fi != fj {
			return fi
		}
		return nodes[i].rank < nodes[j].rank
	})
	for _, n := range nodes {
		w.ready(n)
	}
}

func (w *dagWalk) ready(n *dagNode) {
	if n.readied {
		if n.items != nil && n.pending == 0 && !n.finished {
			w.finishForEach(n)
		}
		return
	}
	n.readied = true
	if !n.continueOnFailure {
		for _, dep := range n.deps {
			if d := w.nodes[dep]; d != nil && d.failed {
				w.skip(n, dep)
				return
			}
		}
	}
	if n.forEach != nil {
		w.expandNode(n)
		return
	}
	if n.limit != nil && n.limit.max > 0 && n.limit.active >= n.limit.max {
		n.limit.queue = append(n.limit.queue, n)
		return
	}
	w.launch(n)
}

func (w *dagWalk) skip(n *dagNode, failedDep string) {
	if w.hooks.onDepFailed != nil {
		w.hooks.onDepFailed(n.name, failedDep)
	}
	w.finish(n, true)
}

func (w *dagWalk) launch(n *dagNode) {
	if n.limit != nil {
		n.limit.active++
	}
	w.running++
	go func() {
		w.done <- dagResult{node: n, err: w.hooks.run(n.name)}
	}()
}

func (w *dagWalk) finish(n *dagNode, failed bool) {
	n.finished, n.failed = true, failed
	var ready []*dagNode
	for _, d := range n.dependents {
		d.pending--
		if d.pending == 0 {
			ready = append(ready, d)
		}
	}
	w.makeReady(ready)
}

// expandNode replaces a for_each node with its items. Each item waits on
// the node's own deps, except that a dep on another for_each node over
// the same source pairs up: report[k] waits only on review[k].
func (w *dagWalk) expandNode(n *dagNode) {
	fe := n.forEach
	var keys []string
	var err error
	if w.hooks.expand != nil {
		keys, err = w.hooks.expand(n.name)
	}
	if err != nil {
		if w.firstErr == nil {
			w.firstErr = err
		}
		n.forEach = nil
		w.finish(n, true)
		return
	}
	n.forEach = nil
	n.items = []*dagNode{}

	// Expand same-source for_each deps first so their items exist to pair with.
	for _, dep := range w.spec.deps[n.name] {
		if d := w.nodes[dep]; d != nil && d.forEach != nil && d.forEach.source == fe.source && d.pending == 0 {
			w.ready(d)
		}
	}

	limit := &dagLimit{max: fe.maxParallel}
	var ready []*dagNode
	for _, key := range keys {
		item := &dagNode{name: instanceName(n.name, key), rank: n.rank, continueOnFailure: n.continueOnFailure, limit: limit}
		for _, dep := range w.spec.deps[n.name] {
			if dep == fe.source {
				continue
			}
			d := w.nodes[dep]
			if d != nil && (d.items != nil || d.forEach != nil) && w.sameSource(dep, fe.source) {
				// Pair with the matching item. None means it was skipped
				// (already succeeded in the run being retried).
				if pair := w.nodes[instanceName(dep, key)]; pair != nil {
					item.addDep(pair)
				}
				continue
			}
			item.deps = append(item.deps, dep)
			if d == nil {
				item.pending++ // unknown: never finishes, like any unknown dep
				continue
			}
			item.addDep(d)
		}
		w.nodes[item.name] = item
		n.items = append(n.items, item)
		item.dependents = append(item.dependents, n)
		n.pending++
		if item.pending == 0 {
			ready = append(ready, item)
		}
	}
	if n.pending == 0 {
		w.finishForEach(n)
		return
	}
	w.makeReady(ready)
}

func (item *dagNode) addDep(d *dagNode) {
	if !contains(item.deps, d.name) {
		item.deps = append(item.deps, d.name)
	}
	if !d.finished {
		item.pending++
		d.dependents = append(d.dependents, item)
	}
}

// sameSource reports whether for_each node name reads source, so its
// items pair with another's by key. Static collections never pair.
func (w *dagWalk) sameSource(name, source string) bool {
	fe, ok := w.spec.forEach[name]
	return ok && source != "" && fe.source == source
}

func (w *dagWalk) finishForEach(n *dagNode) {
	failed := 0
	for _, it := range n.items {
		if it.failed {
			failed++
		}
	}
	if w.hooks.onForEachDone != nil {
		w.hooks.onForEachDone(n.name, len(n.items), failed)
	}
	w.finish(n, failed > 0)
}

// dagDepths is each node's longest path from a root (cycles count as 0).
func dagDepths(deps map[string][]string) map[string]int {
	depth := make(map[string]int, len(deps))
	visiting := map[string]bool{}
	var of func(string) int
	of = func(n string) int {
		if d, ok := depth[n]; ok {
			return d
		}
		if visiting[n] {
			return 0
		}
		visiting[n] = true
		d := 0
		for _, p := range deps[n] {
			if _, known := deps[p]; known {
				d = max(d, of(p)+1)
			}
		}
		visiting[n] = false
		depth[n] = d
		return d
	}
	for n := range deps {
		of(n)
	}
	return depth
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// scratchDirFor computes the absolute scratch dir for one schedule run.
// Lives under <croniclePath>/.cronicle/scratch/<schedule>/<run-id>/ so
// concurrent runs of the same schedule don't collide and each run is
// addressable by id. Returns "" if the schedule has no tasks (no
// croniclePath to resolve), in which case dag.go skips the mkdir and
// ${scratch} substitution becomes a no-op.
//
// Prefers schedule.RunID (a ULID) over the truncated-to-second timestamp.
// Two runs of the same schedule firing within the same second (e.g. a
// cron tick coinciding with an HTTP trigger, or two concurrent triggers)
// used to produce the same path and clobber each other's files. The
// timestamp form is kept only as a fallback for paths that don't carry
// a RunID (direct in-process callers from very old code).
func (schedule *Schedule) scratchDirFor(now time.Time) string {
	if len(schedule.Tasks) == 0 {
		return ""
	}
	croniclePath := schedule.Tasks[0].CroniclePath
	if croniclePath == "" {
		return ""
	}
	dirName := schedule.RunID
	if dirName == "" {
		dirName = now.UTC().Format("20060102T150405Z")
	}
	return filepath.Join(croniclePath, ".cronicle", "scratch", schedule.Name, dirName)
}

// downstreamTasks returns the transitive set of tasks that depend on
// `root` within the schedule's DAG. Used by the task-cancel listener
// endpoint to compute the cascade set — every downstream task of the
// canceled task is also marked canceled in the projection so the DAG
// walker skips them at launch time.
//
// Returned in deterministic insertion order (BFS layer-by-layer) so
// downstream consumers (logs, audit lines) see a stable ordering.
// `root` itself is NOT included; callers prepend it explicitly when
// they want the full target set.
func (schedule Schedule) downstreamTasks(root string) []string {
	dependents := make(map[string][]string)
	for _, t := range schedule.Tasks {
		for _, dep := range t.Depends {
			dependents[dep] = append(dependents[dep], t.Name)
		}
	}
	var out []string
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range dependents[cur] {
			if seen[child] {
				continue
			}
			seen[child] = true
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}

// downstreamOf is downstreamTasks for any task name, including an item of
// a for_each task: review[12]'s downstream is the matching item of each
// for_each task over the same source (report[12]). Tasks that fan in over
// every item aren't included — they still run once the other items finish.
func (schedule Schedule) downstreamOf(name string) []string {
	base, key, ok := splitInstanceName(name)
	if !ok {
		return schedule.downstreamTasks(name)
	}
	byName := schedule.TaskMap()
	src := ""
	if fe := byName[base].ForEach; fe != nil {
		src = fe.Source
	}
	var out []string
	for _, d := range schedule.downstreamTasks(base) {
		if fe := byName[d].ForEach; src != "" && fe != nil && fe.Source == src {
			out = append(out, instanceName(d, key))
		}
	}
	return out
}

// dependsAttr encodes each task's dependencies as a comma-joined string,
// index-aligned with the schedule_start `tasks` attr ("" for a root). A
// []string survives the worker→runner JSON round-trip (rehydrateRecord)
// with its kind intact, where a map would collapse to a JSON blob — so
// the pretty renderer can draw the real DAG on both stdout and the SSE wire.
func dependsAttr(taskNames []string, deps map[string][]string) []string {
	out := make([]string, len(taskNames))
	for i, name := range taskNames {
		out[i] = strings.Join(deps[name], ",")
	}
	return out
}

// dagString produces a human-readable representation of the DAG for logging,
// in task declaration order so the output is stable run to run.
func dagString(taskNames []string, deps map[string][]string) string {
	var sb strings.Builder
	sb.WriteString("DAG:\n")
	for _, node := range taskNames {
		nodeDeps := deps[node]
		if len(nodeDeps) == 0 {
			fmt.Fprintf(&sb, "  %s\n", node)
		} else {
			fmt.Fprintf(&sb, "  %s -> [%s]\n", node, strings.Join(nodeDeps, ", "))
		}
	}
	return sb.String()
}

// logTaskError records a task that failed before it could run (a bad
// for_each collection, or an item missing a field the task reads).
func logTaskError(schedule Schedule, task string, err error) {
	slog.Error("task failed",
		"entry_type", "task_error",
		"run_id", schedule.RunID,
		"schedule", schedule.Name,
		"task", task,
		"error", err.Error(),
		"success", false,
	)
}
