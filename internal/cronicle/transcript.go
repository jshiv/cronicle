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
// .cronicle/runs/{ts}-{schedule}-{task}.jsonl. Returns "" if file logging is
// disabled. The schema mirrors the agent transcript: three lines (request /
// response / accounting), so consumers can read both task types uniformly.
func writeShellTranscript(scheduleName, taskName, taskPath string, env []string,
	started, finished time.Time, res exec.Result,
	gitCommit, gitAuthor string) (string, error) {
	if !FileLoggingEnabled {
		return "", nil
	}
	runsDir := TranscriptDir()
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%s.jsonl",
		started.UTC().Format("20060102T150405Z"), scheduleName, taskName)
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
