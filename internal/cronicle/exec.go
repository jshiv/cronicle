package cronicle

import (
	"context"
	"fmt"
	"io"
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

		streaming := IsStreamingPretty()
		var sw *StreamingWriter
		var stdoutW, stderrW io.Writer
		if streaming {
			// Per-task writer: leader streams to stdout, followers buffer.
			// The shell command runs WITHOUT holding the global lock, so
			// parallel shell tasks still overlap their compute.
			sw = NewStreamingWriter()
			defer sw.Close()
			WriteShellRunHeader(sw, task.ScheduleName, task.Name, strings.Join(cmd, " "))
			fmt.Fprintln(sw)
			stdoutW = sw
			stderrW = sw
		}

		startedAt := time.Now()
		if streaming {
			result = exec.ExecuteWithStream(cmd, task.Path, task.Env, stdoutW, stderrW)
		} else {
			result = exec.Execute(cmd, task.Path, task.Env)
		}
		finishedAt := time.Now()
		task.lastDurationMs = finishedAt.Sub(startedAt).Milliseconds()

		if FileLoggingEnabled {
			commit, email := task.gitMeta()
			if path, err := writeShellTranscript(task.ScheduleName, task.Name, task.Path,
				task.Env, startedAt, finishedAt, result, commit, email); err == nil {
				task.lastTranscript = path
			}
		}

		if streaming {
			fmt.Fprintln(sw)
			WriteShellRunFooter(sw,
				int64(result.ExitStatus), task.lastDurationMs, task.lastTranscript)
			task.shellStreamed = true
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

	// Skills: load all listed SKILL.md files up-front so a misconfigured
	// skill (missing file, bad frontmatter) fails the run before any API
	// call. The frontmatter (name+description) builds the available-skills
	// catalog appended to the system prompt; bodies stay out until the
	// agent calls load_skill (progressive disclosure, per Anthropic's
	// Skills standard).
	skills, skillErr := LoadSkillsForTask(task.Path, task.Agent.Skills)
	if skillErr != nil {
		return exec.Result{
			Command:    []string{"agent", task.Agent.Model},
			Error:      skillErr,
			Stderr:     skillErr.Error(),
			ExitStatus: 1,
		}
	}
	var skillTool *SkillTool
	if len(skills) > 0 {
		skillTool, skillErr = NewSkillTool(skills)
		if skillErr != nil {
			return exec.Result{
				Command:    []string{"agent", task.Agent.Model},
				Error:      skillErr,
				Stderr:     skillErr.Error(),
				ExitStatus: 1,
			}
		}
	}

	system := r.Replace(task.Agent.System)
	if section := FormatAvailableSkillsSection(skills); section != "" {
		if system != "" {
			system += "\n\n"
		}
		system += section
	}

	cfg := agent.Config{
		Prompt:        r.Replace(task.Agent.Prompt),
		System:        system,
		Model:         task.Agent.Model,
		MaxTokens:     task.Agent.MaxTokens,
		BudgetUSD:     task.Agent.BudgetUSD,
		MaxTurns:      task.Agent.MaxTurns,
		TranscriptDir: TranscriptDir(), // "" unless --log-to-file
		RunID:         runID,
	}

	streaming := IsStreamingPretty()
	effectiveModel := cfg.Model
	if effectiveModel == "" {
		effectiveModel = agent.DefaultModel
	}
	var skillsAvailable []string
	if skillTool != nil {
		skillsAvailable = skillTool.Available()
	}
	var sw *StreamingWriter
	if streaming {
		// Per-task writer: the leader streams live to stdout; followers
		// buffer and atomically flush on Close. agent.Run runs WITHOUT
		// holding the global lock, so parallel tasks still execute
		// concurrently.
		sw = NewStreamingWriter()
		defer sw.Close()
		WriteAgentRunHeader(sw, task.ScheduleName, task.Name, effectiveModel, skillsAvailable)
		fmt.Fprintln(sw)
		cfg.StreamHandler = NewAgentStreamRenderer(sw)
	}

	// Tools: bind to the task's workspace and stream writer so bash output
	// flows into the agent's pretty block AND the cronicle execution
	// context stays consistent (same cwd as a shell task would have).
	var toolWriter io.Writer
	if streaming {
		toolWriter = sw
	}
	cfg.Tools = buildAgentTools(task.Agent.Tools, task.Path, task.Env, toolWriter)
	if skillTool != nil {
		cfg.Tools = append(cfg.Tools, skillTool)
	}

	// Wallclock bound: derive a context deadline from the HCL string. Default
	// 10m if unset. Wallclock is enforced via context cancellation; the agent
	// loop will return whatever's been gathered so far when the deadline fires.
	wallclock := 10 * time.Minute
	if task.Agent.Wallclock != "" {
		if d, err := time.ParseDuration(task.Agent.Wallclock); err == nil {
			wallclock = d
		}
	}
	runCtx, cancel := context.WithTimeout(context.Background(), wallclock)
	defer cancel()

	// MCP servers: launch under runCtx so wallclock cancellation also tears
	// them down. Failures here abort the run before any API call. We close
	// handles in the deferred path below regardless of how the run exits,
	// so a panic or budget abort still cleans up subprocesses.
	mcpHandles, mcpTools, mcpErr := LaunchMCPServers(runCtx, task.Agent.MCPs, task.Env, toolWriter)
	if mcpErr != nil {
		return exec.Result{
			Command:    []string{"agent", task.Agent.Model},
			Error:      mcpErr,
			Stderr:     mcpErr.Error(),
			ExitStatus: 1,
		}
	}
	defer func() {
		for _, h := range mcpHandles {
			_ = h.Close()
		}
	}()
	cfg.Tools = append(cfg.Tools, mcpTools...)

	startedAt := time.Now()
	res, err := agent.Run(runCtx, cfg)
	durationMs := time.Since(startedAt).Milliseconds()

	if streaming {
		fmt.Fprintln(sw)
		fmt.Fprintln(sw)
		WriteAgentRunFooter(sw,
			int64(res.InputTokens), int64(res.OutputTokens), int64(res.CacheReadIn),
			fmt.Sprintf("%.6f", res.CostUSD), durationMs,
			res.StopReason, res.TranscriptPath)
	}

	streamingEntryType := "agent_run"
	if streaming {
		streamingEntryType = "agent_run_streamed"
	}

	attrs := []slog.Attr{
		slog.String("entry_type", streamingEntryType),
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
	if skillTool != nil {
		// skills_available: the catalog the agent saw.
		// skills_loaded:    the subset whose bodies it actually fetched.
		// Diff of the two answers "did the run use what it had?".
		attrs = append(attrs,
			slog.Any("skills_available", skillsAvailable),
			slog.Any("skills_loaded", skillTool.Loaded()))
	}
	if names := MCPServerNames(mcpHandles); len(names) > 0 {
		attrs = append(attrs, slog.Any("mcp_servers", names))
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

		slog.Info("task started",
			"entry_type", "task_start",
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

	entryType := "shell_run"
	if task.shellStreamed {
		entryType = "shell_run_streamed"
	}

	args := []any{
		"entry_type", entryType,
		"schedule", task.ScheduleName,
		"task", task.Name,
		"path", task.Path,
		"exit", res.ExitStatus,
		"commit", commit,
		"email", email,
		"command", strings.Join(res.Command, " "),
		"duration_ms", task.lastDurationMs,
		"stdout", res.Stdout,
		"stderr", res.Stderr,
	}
	if task.lastTranscript != "" {
		args = append(args, "transcript", task.lastTranscript)
	}

	if res.Error != nil {
		args = append(args, "error", res.Error.Error(), "success", false)
		slog.Error("shell task failed", args...)
	} else {
		args = append(args, "success", true)
		slog.Info("shell task complete", args...)
	}
}
