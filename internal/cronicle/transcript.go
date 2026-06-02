package cronicle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jshiv/cronicle/pkg/exec"
)

// redactEnv returns a copy of env with each entry's value replaced by
// "***". Keys are preserved so the transcript still shows which vars
// the subprocess received; values are dropped because they may contain
// secrets resolved by HCL's ${env.X} interpolation at parse time, or
// because the operator wrote a literal secret in their HCL file. A
// transcript on disk is a more durable exposure than HCL in process
// memory, so default to safety here.
func redactEnv(env []string) []string {
	if len(env) == 0 {
		return env
	}
	out := make([]string, len(env))
	for i, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			out[i] = k + "=***"
		} else {
			out[i] = kv
		}
	}
	return out
}

// writeShellTranscript writes a per-run JSONL transcript for a shell task to
// .cronicle/runs/{run_id}-{task}.jsonl when runID is set (the listener-fired
// path), else .cronicle/runs/{ts}-{schedule}-{task}.jsonl (legacy / direct
// `cronicle exec` path). Returns "" if file logging is disabled. The schema
// mirrors the agent transcript: three lines (request / response / accounting),
// so consumers can read both task types uniformly. Keying by the run_id makes
// the file findable from the listener's /v1/runs/{id} response without a
// schedule+task lookup.
func writeShellTranscript(runID, scheduleName, taskName, taskPath string, env []string,
	started, finished time.Time, res exec.Result,
	gitCommit, gitAuthor string) (string, error) {
	if !FileLoggingEnabled {
		return "", nil
	}
	runsDir := TranscriptDir()
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return "", err
	}
	var name string
	if runID != "" {
		name = fmt.Sprintf("%s-%s.jsonl", runID, taskName)
	} else {
		name = fmt.Sprintf("%s-%s-%s.jsonl",
			started.UTC().Format("20060102T150405Z"), scheduleName, taskName)
	}
	p := filepath.Join(runsDir, name)
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	_ = enc.Encode(map[string]any{
		"type":       "request",
		"started_at": started.UTC(),
		"command":    res.Command,
		"env":        redactEnv(env),
		"path":       taskPath,
	})
	_ = enc.Encode(map[string]any{
		"type":        "response",
		"finished_at": finished.UTC(),
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
		"exit_status": res.ExitStatus,
	})
	_ = enc.Encode(map[string]any{
		"type":        "accounting",
		"duration_ms": finished.Sub(started).Milliseconds(),
		"git_commit":  gitCommit,
		"git_author":  gitAuthor,
	})
	return p, nil
}
