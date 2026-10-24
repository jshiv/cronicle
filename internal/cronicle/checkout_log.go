package cronicle

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// A task with its own repo checks it out (clone or open, fetch, checkout)
// before it runs. Those steps are part of the task, so they're drawn in
// its block, under a "git" divider, rather than as loose log lines
// between blocks:
//
//	╭─ review[12]  agent · pr_review · claude-sonnet-4-6
//	│ prompt    Review pull request #12 …
//	├─ git ──────────────
//	│ clone https://github.com/org/app.git
//	│ checkout commit 335e31b
//	│ at 335e31b Handle empty input in average
//	├────────────────────
//
// checkoutLog is the task's git logger. Each step is still logged as a
// task_checkout record (text and JSON logs, the run's file log), and is
// also kept as a short line for the block; the pretty renderers skip the
// records and draw the lines instead.
type checkoutLog struct {
	mu    sync.Mutex
	lines []string
}

// logger returns a slog.Logger that records into c and tags each record
// with the task.
func (c *checkoutLog) logger(task *Task) *slog.Logger {
	return slog.New(checkoutHandler{log: c, next: slog.Default().Handler(), attrs: []slog.Attr{
		slog.String("entry_type", "task_checkout"),
		slog.String("run_id", task.RunID),
		slog.String("schedule", task.ScheduleName),
		slog.String("task", task.Name),
	}})
}

func (c *checkoutLog) add(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

// record turns a git.go log record into the block's short form.
func (c *checkoutLog) record(r slog.Record) {
	attrs := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.Message {
	case "git clone":
		c.lines = append(c.lines, "clone "+attrs["url"])
	case "git clone complete":
		// implied by the clone line
	case "git open":
		c.lines = append(c.lines, "existing checkout")
	case "git fetch":
		c.lines = append(c.lines, "fetch")
	case "git fetch: already up to date":
		if n := len(c.lines); n > 0 && c.lines[n-1] == "fetch" {
			c.lines[n-1] = "fetch: already up to date"
		} else {
			c.lines = append(c.lines, "fetch: already up to date")
		}
	case "git checkout":
		kind, ref, _ := strings.Cut(attrs["target"], "=")
		c.lines = append(c.lines, "checkout "+kind+" "+shortSHA(ref))
	default:
		line := strings.TrimPrefix(r.Message, "git ")
		if e := attrs["error"]; e != "" {
			line += ": " + e
		}
		c.lines = append(c.lines, line)
	}
}

func (c *checkoutLog) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// shortSHA abbreviates a full commit hash; anything else (a branch) is
// left as is.
func shortSHA(ref string) string {
	if len(ref) == 40 && strings.Trim(ref, "0123456789abcdef") == "" {
		return ref[:7]
	}
	return ref
}

// checkoutHandler records into a checkoutLog and passes each record on,
// with the task's attrs, to the normal handlers.
type checkoutHandler struct {
	log   *checkoutLog
	next  slog.Handler
	attrs []slog.Attr
}

func (h checkoutHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h checkoutHandler) Handle(ctx context.Context, r slog.Record) error {
	h.log.record(r)
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	r = r.Clone()
	r.AddAttrs(h.attrs...)
	return h.next.Handle(ctx, r)
}

func (h checkoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return checkoutHandler{log: h.log, next: h.next, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h checkoutHandler) WithGroup(name string) slog.Handler {
	return checkoutHandler{log: h.log, next: h.next.WithGroup(name), attrs: h.attrs}
}
