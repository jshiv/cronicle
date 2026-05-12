package cronicle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jshiv/cronicle/pkg/exec"
)

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
		"env":        env,
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
