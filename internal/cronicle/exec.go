package cronicle

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jshiv/cronicle/pkg/agent"
	"github.com/jshiv/cronicle/pkg/exec"
	"gopkg.in/matryer/try.v1"
)

//Exec executes task.Command at task.Path and returns the exec.Result struct
//prior to execution, the command will replace any ${date}, ${datetime}, ${timestamp}
//with time t given in the bash command. If task.Agent is set, the task is
//dispatched to pkg/agent instead.
func (task *Task) Exec(t time.Time) exec.Result {
	r := strings.NewReplacer(
		"${date}", t.Format(TimeArgumentFormatMap["${date}"]),
		"${datetime}", t.Format(TimeArgumentFormatMap["${datetime}"]),
		"${timestamp}", t.Format(TimeArgumentFormatMap["${timestamp}"]),
		"${path}", task.Path,
	)

	if task.Agent != nil {
		return task.execAgent(t, r)
	}

	var result exec.Result
	if len(task.Command) > 0 {
		cmd := make([]string, len(task.Command))
		for i, s := range task.Command {
			s = r.Replace(s)
			cmd[i] = s
		}

		startedAt := time.Now()
		result = exec.Execute(cmd, task.Path, task.Env)
		finishedAt := time.Now()
		task.lastDurationMs = finishedAt.Sub(startedAt).Milliseconds()

		if FileLoggingEnabled {
			commit, email := task.gitMeta()
			if path, err := writeShellTranscript(task.ScheduleName, task.Name, task.Path,
				task.Env, startedAt, finishedAt, result, commit, email); err == nil {
				task.lastTranscript = path
			}
		}
	}
	return result
}

// gitMeta extracts the commit hash and author email from task.Git, returning
// "null" placeholders when no commit is attached.
func (task *Task) gitMeta() (commit, email string) {
	if task.Git.Commit != nil {
		commit = task.Git.Commit.Hash.String()[:11]
		email = task.Git.Commit.Author.Email
		return
	}
	return "null", "null"
}

// execAgent dispatches the task to pkg/agent and emits a single structured
// log entry covering the run. The entry carries entry_type="agent_run" so the
// pretty formatter can render it as a multi-line block; in text/json mode the
// fields stay on one line. task.Log is skipped for agent tasks because this
// function owns the agent's logging end-to-end.
func (task *Task) execAgent(t time.Time, r *strings.Replacer) exec.Result {
	runID := fmt.Sprintf("%s-%s-%s", t.UTC().Format("20060102T150405Z"), task.ScheduleName, task.Name)

	cfg := agent.Config{
		Prompt:        r.Replace(task.Agent.Prompt),
		System:        r.Replace(task.Agent.System),
		Model:         task.Agent.Model,
		MaxTokens:     task.Agent.MaxTokens,
		BudgetUSD:     task.Agent.BudgetUSD,
		TranscriptDir: TranscriptDir(), // "" unless --log-to-file
		RunID:         runID,
	}

	startedAt := time.Now()
	res, err := agent.Run(context.Background(), cfg)
	durationMs := time.Since(startedAt).Milliseconds()

	attrs := []slog.Attr{
		slog.String("entry_type", "agent_run"),
		slog.String("schedule", task.ScheduleName),
		slog.String("task", task.Name),
		slog.String("model", res.Model),
		slog.Int("input_tokens", res.InputTokens),
		slog.Int("output_tokens", res.OutputTokens),
		slog.Int("cache_read", res.CacheReadIn),
		slog.Int("cache_write", res.CacheWriteIn),
		slog.String("cost_usd", fmt.Sprintf("%.6f", res.CostUSD)),
		slog.Int64("duration_ms", durationMs),
		slog.String("stop_reason", res.StopReason),
		slog.String("response", res.Stdout),
	}
	if res.TranscriptPath != "" {
		attrs = append(attrs, slog.String("transcript", res.TranscriptPath))
	}
	if err != nil {
		attrs = append(attrs, slog.Bool("success", false), slog.String("error", err.Error()))
		slog.LogAttrs(context.Background(), slog.LevelError, "agent run failed", attrs...)
	} else {
		attrs = append(attrs, slog.Bool("success", true))
		slog.LogAttrs(context.Background(), slog.LevelInfo, "agent run", attrs...)
	}
	return res.Result
}

// Execute does a git pull, git checkout and exec's the given command
func (task *Task) Execute(t time.Time) (exec.Result, error) {

	//Validate the task
	if err := task.Validate(); err != nil {
		return exec.Result{}, err
	}

	//Test if the given task should execute in the root croniclePath and the croncilePath is a git repo
	taskPathIsCroniclePathWithGit := (task.Path == task.CroniclePath) && task.CronicleRepo != nil

	//If a repo is given, clone the repo and task.Git.Open(task.Path)
	if task.Repo != nil {
		auth, err := task.Repo.Auth()
		if err != nil {
			return exec.Result{}, err
		}
		g, err := Clone(task.Path, task.Repo.URL, &auth)
		// g, err := Clone(task.Path, task.Repo.URL, task.Repo.DeployKey)
		if err != nil {
			return exec.Result{}, err
		}
		task.Git = g
		err = task.Git.Checkout(task.Repo.Branch, task.Repo.Commit)
		if err != nil {
			return exec.Result{}, err
		}
	} else if taskPathIsCroniclePathWithGit {
		auth, err := task.CronicleRepo.Auth()
		if err != nil {
			return exec.Result{}, err
		}
		task.Git, err = Clone(task.CroniclePath, task.CronicleRepo.URL, &auth)
		// var err error
		// task.Git, err = Clone(task.CroniclePath, task.CronicleRepo.URL, task.CronicleRepo.DeployKey)
		if err != nil {
			slog.Error("clone failed", "error", err.Error())
			return exec.Result{}, err
		}
	}

	//Execute task.Command in bash at time t with retry
	var result exec.Result
	err := try.Do(func(attempt int) (bool, error) {

		slog.Info("Executing...",
			"schedule", task.ScheduleName,
			"task", task.Name,
			"attempt", attempt,
			"clock", t.Format(time.Kitchen),
			"date", t.Format(time.RFC850),
		)
		var err error
		result = task.Exec(t)
		err = result.Error
		task.Log(result)
		if err != nil && task.Retry != nil {
			duration := time.Duration(task.Retry.Seconds) * time.Second
			duration += time.Duration(task.Retry.Minutes) * time.Minute
			duration += time.Duration(task.Retry.Hours) * time.Hour
			time.Sleep(duration)
		}

		var retryCount int
		switch task.Retry {
		case nil:
			retryCount = 0
		default:
			retryCount = task.Retry.Count
		}

		return attempt < retryCount, err
	})
	if err != nil {
		return result, err
	}

	return result, nil
}

//Log logs the exit status, stderr, git commit and other logging data.
//Agent tasks own their logging end-to-end via execAgent, so this is a no-op
//for them.
func (task *Task) Log(res exec.Result) {
	if task.Agent != nil {
		return
	}

	commit, email := task.gitMeta()

	args := []any{
		"schedule", task.ScheduleName,
		"task", task.Name,
		"path", task.Path,
		"exit", res.ExitStatus,
		"commit", commit,
		"email", email,
		"command", strings.Join(res.Command, " "),
		"duration_ms", task.lastDurationMs,
	}
	if task.lastTranscript != "" {
		args = append(args, "transcript", task.lastTranscript)
	}

	if res.Error != nil {
		args = append(args, "error", res.Error.Error(), "success", false)
		slog.Error(res.Stderr, args...)
	} else {
		args = append(args, "success", true)
		slog.Info(res.Stdout, args...)
	}
}
