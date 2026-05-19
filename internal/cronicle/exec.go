package cronicle

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/jshiv/cronicle/internal/cronicle/secretstore"
	"github.com/jshiv/cronicle/pkg/agent"
	"github.com/jshiv/cronicle/pkg/exec"
	"gopkg.in/matryer/try.v1"
)

// splitAPIKey pulls an ANTHROPIC_API_KEY entry out of the env slice
// and returns (value, remainder). Used by the agent dispatch path so
// the Anthropic API key flows to cfg.APIKey (which is passed to the
// SDK via option.WithAPIKey) without ever needing to be in any
// subprocess's env. Returns ("", env) when no such entry is present
// — the agent then falls back to whatever the SDK's own env lookup
// finds, which is fine for direct-run scenarios where the operator
// set ANTHROPIC_API_KEY on cronicle's own process.
func splitAPIKey(env []string) (apiKey string, rest []string) {
	const prefix = "ANTHROPIC_API_KEY="
	rest = make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) && apiKey == "" {
			apiKey = entry[len(prefix):]
			continue
		}
		rest = append(rest, entry)
	}
	return apiKey, rest
}

// resolveEnv expands $secret.NAME references in env using the
// process-wide secret store. Behavior matrix:
//
//	store not configured → env passed through unchanged (no expansion
//	                       attempted). Lets a cronicle run without a
//	                       --secret-source flag keep working — values
//	                       that happen to contain "$secret.X" pass
//	                       through as literals, same as today.
//	store configured     → expansion required; any unresolved reference
//	                       fails the task before subprocesses spawn.
//
// The second mode is the right default once secrets are wired —
// dispensing a stale literal "$secret.X" string to a tool would
// silently produce broken behavior (auth failure, etc.), and the
// store-configured signal is the operator's commitment that
// references should resolve.
func resolveEnv(env []string) ([]string, error) {
	if len(env) == 0 {
		return env, nil
	}
	store := secretstore.Default()
	if !store.Configured() {
		return env, nil
	}
	return store.ExpandEnv(env)
}

//Exec executes task.Command at task.Path and returns the exec.Result struct
//prior to execution, the command will replace any ${date}, ${datetime}, ${timestamp}
//with time t given in the bash command. If task.Agent is set, the task is
//dispatched to pkg/agent instead.
func (task *Task) Exec(t time.Time) exec.Result {
	// Reset per-attempt state. Execute() loops on retry by re-calling Exec
	// on the same *Task, so without this any field that an early-return
	// path leaves unset would carry forward from the previous attempt
	// (e.g. a transcript path from a successful first run leaking into a
	// failing-then-skipped retry's log).
	task.lastTranscript = ""
	task.lastDurationMs = 0
	task.shellStreamed = false

	r := strings.NewReplacer(
		"${date}", t.Format(TimeArgumentFormatMap["${date}"]),
		"${datetime}", t.Format(TimeArgumentFormatMap["${datetime}"]),
		"${timestamp}", t.Format(TimeArgumentFormatMap["${timestamp}"]),
		"${path}", task.Path,
		// ${scratch} is the schedule-scoped shared dir tasks use to pass
		// artifacts to downstream tasks (set by Schedule.ExecuteTasks).
		// Empty string when the schedule wasn't routed through ExecuteTasks
		// (e.g. one-off `cronicle exec`) — the substitution leaves the
		// literal in place, which is loud enough to debug.
		"${scratch}", task.ScratchDir,
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
		var stdoutInner, stderrInner io.Writer = io.Discard, io.Discard
		if streaming {
			// Per-task writer: leader streams to stdout, followers buffer.
			// The shell command runs WITHOUT holding the global lock, so
			// parallel shell tasks still overlap their compute.
			sw = NewStreamingWriter()
			defer sw.Close()
			WriteShellRunHeader(sw, task.ScheduleName, task.Name, strings.Join(cmd, " "))
			fmt.Fprintln(sw)
			stdoutInner = sw
			stderrInner = sw
		}

		// Per-line slog emission feeds LiveSink (live SSE) and the JSONL
		// log (→ Loki) regardless of stdout pretty mode. The frontend's
		// live pane works even when cronicle's stdout is text/json.
		stdoutEmitter := newLineEmitter(stdoutInner, task.RunID, task.Name, task.ScheduleName, "stdout")
		stderrEmitter := newLineEmitter(stderrInner, task.RunID, task.Name, task.ScheduleName, "stderr")
		defer stdoutEmitter.Flush()
		defer stderrEmitter.Flush()

		// Header slog record carries the metadata pretty stdout would put
		// in its bordered block (schedule, task, command). LiveSink's
		// pretty encoder renders it as the header; stdout pretty mode
		// suppresses it because StreamingWriter already wrote those bytes
		// directly (above). file/Loki consumers see a structural record
		// they can use to mark "task started" with full context.
		slog.Info("shell run start",
			"entry_type", "shell_run_start",
			"run_id", task.RunID,
			"schedule", task.ScheduleName,
			"task", task.Name,
			"command", strings.Join(cmd, " "),
		)

		// Honor the per-run cancel context when one is plumbed in
		// (HTTP-worker mode, /v1/runs/{id}/cancel preempt). Falls
		// through to context.Background() in default in-process mode
		// where the run is foreground and cancel isn't applicable.
		runCtx := task.RunCtx
		if runCtx == nil {
			runCtx = context.Background()
		}
		startedAt := time.Now()
		// Resolve $secret.NAME references against the live secret store
		// before handing env to the subprocess. With no secret source
		// configured this is a passthrough; with one configured, the
		// secret values are substituted in-place. Either way, the
		// task.Env stored on the Task struct stays unchanged so HCL
		// serialization round-trips cleanly.
		shellEnv, secretErr := resolveEnv(task.Env)
		if secretErr != nil {
			result = exec.Result{
				Command:    cmd,
				Error:      secretErr,
				Stderr:     secretErr.Error(),
				ExitStatus: 1,
			}
			return result
		}
		// Always use the streaming entry point so per-line slog records
		// fire; pkg/exec captures stdout/stderr into the Result either
		// way, so the shell_run terminal event payload is unchanged.
		result = exec.ExecuteWithStreamContext(runCtx, cmd, task.Path, shellEnv, stdoutEmitter, stderrEmitter)
		finishedAt := time.Now()
		task.lastDurationMs = finishedAt.Sub(startedAt).Milliseconds()

		if FileLoggingEnabled {
			commit, email := task.gitMeta()
			if path, err := writeShellTranscript(task.RunID, task.ScheduleName, task.Name, task.Path,
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
	// Prefer the listener-assigned per-fire run_id so the transcript filename
	// is findable from /v1/runs/{id} ({run_id}-{task}.jsonl). Fall back to the
	// timestamp-derived form when running outside the listener (e.g. direct
	// `cronicle exec` against a local file).
	var runID string
	if task.RunID != "" {
		runID = fmt.Sprintf("%s-%s", task.RunID, task.Name)
	} else {
		runID = fmt.Sprintf("%s-%s-%s", t.UTC().Format("20060102T150405Z"), task.ScheduleName, task.Name)
	}

	// Skills: load all listed SKILL.md files up-front so a misconfigured
	// skill (missing file, bad frontmatter) fails the run before any API
	// call. The frontmatter (name+description) builds the available-skills
	// catalog appended to the system prompt; bodies stay out until the
	// agent calls load_skill (progressive disclosure, per Anthropic's
	// Skills standard).
	skills, skillErr := LoadSkillsForTask(task.Path, task.CroniclePath, task.Agent.Skills)
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
	// Resolve template vars in tools list (no-op today; future-proof) and
	// in MCP command lines so the operator sees what actually got spawned.
	// Mcp command expansion is repeated below for the LaunchMCPServers call;
	// we compute it once for the header and reuse the resolved values.
	resolvedTools := append([]string(nil), task.Agent.Tools...)
	resolvedMCPCommands := make([]string, 0, len(task.Agent.MCPs))
	for _, m := range task.Agent.MCPs {
		expanded := make([]string, len(m.Command))
		for j, c := range m.Command {
			expanded[j] = r.Replace(c)
		}
		resolvedMCPCommands = append(resolvedMCPCommands,
			fmt.Sprintf("%s: %s", m.Name, strings.Join(expanded, " ")))
	}
	var sw *StreamingWriter
	var downstream agent.StreamHandler
	if streaming {
		// Per-task writer: the leader streams live to stdout; followers
		// buffer and atomically flush on Close. agent.Run runs WITHOUT
		// holding the global lock, so parallel tasks still execute
		// concurrently.
		sw = NewStreamingWriter()
		defer sw.Close()
		WriteAgentRunHeader(sw, task.ScheduleName, task.Name, effectiveModel, skillsAvailable,
			cfg.Prompt, system, resolvedTools, resolvedMCPCommands)
		fmt.Fprintln(sw)
		downstream = NewAgentStreamRenderer(sw)
	}
	// Always wire the slog bridge so text_delta / thinking_delta /
	// tool_use_start / tool_result / turn_start records flow to
	// LiveSink (live SSE pane) and the JSONL log (→ Loki) regardless
	// of stdout pretty mode. We use task.RunID (the SCHEDULE run_id)
	// for the slog records — that's what /v1/runs/{id}/events keys
	// subscriptions on. The local `runID` is a separate agent-internal
	// identifier used for the per-task transcript file name.
	cfg.StreamHandler = NewAgentSlogBridge(task.RunID, task.Name, task.ScheduleName, downstream)

	// Header slog record — carries schedule/task/model/skills metadata.
	// LiveSink's pretty encoder renders this as the bordered header block;
	// stdout pretty mode suppresses it because WriteAgentRunHeader (above)
	// already wrote those bytes directly. file/Loki consumers see a
	// structural "agent task started" record with full context.
	slog.Info("agent run start",
		"entry_type", "agent_run_start",
		"run_id", task.RunID,
		"schedule", task.ScheduleName,
		"task", task.Name,
		"model", effectiveModel,
		"skills_available", skillsAvailable,
		// Resolved prompt/system: post-${date}/${path}/${scratch} substitution,
		// so the operator can see the exact text the agent received. Same goes
		// for mcp_commands — the launched argv post-expansion. Tools list is
		// echoed as-is (no template expansion today, future-proofed).
		"prompt", cfg.Prompt,
		"system", system,
		"tools", resolvedTools,
		"mcp_commands", resolvedMCPCommands,
	)

	// Tools: bind to the task's workspace and stream writer so bash output
	// flows into the agent's pretty block AND the cronicle execution
	// context stays consistent (same cwd as a shell task would have).
	var toolWriter io.Writer
	if streaming {
		toolWriter = sw
	}
	// git tool reuses the task's repo auth so push works against the same
	// remote the clone came from. Errors here are non-fatal — read-only
	// git operations (status, log, diff) work without auth.
	var gitAuth transport.AuthMethod
	if task.Repo != nil {
		if a, err := task.Repo.Auth(); err == nil {
			gitAuth = a
		}
	}
	// Resolve $secret.NAME references in the task's env array before
	// any subprocess sees it. resolvedEnv carries plaintext secret
	// values — it's passed to bash/MCP subprocesses via the existing
	// per-tool wiring; it's also where we pull cfg.APIKey from for
	// the agent's API client. The unmodified task.Env stays on the
	// Task struct so HCL round-trips and so audit logging never gets
	// plaintext values via that field.
	resolvedEnv, secretErr := resolveEnv(task.Env)
	if secretErr != nil {
		return exec.Result{
			Command:    []string{"agent", task.Agent.Model},
			Error:      secretErr,
			Stderr:     secretErr.Error(),
			ExitStatus: 1,
		}
	}
	// Pull ANTHROPIC_API_KEY (if present in resolved env) out and into
	// cfg.APIKey explicitly. This means we DON'T have to put it in the
	// agent process's env to make the SDK happy, and we DON'T leak it
	// into the bash tool / MCP children that get resolvedEnv passed
	// through. Other env entries continue to flow to subprocesses.
	if apiKey, restEnv := splitAPIKey(resolvedEnv); apiKey != "" {
		cfg.APIKey = apiKey
		resolvedEnv = restEnv
	}
	cfg.Tools = buildAgentTools(task.Agent.Tools, task.Path, resolvedEnv, gitAuth, task.ScratchDir, toolWriter)
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
	parentCtx := task.RunCtx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(parentCtx, wallclock)
	defer cancel()

	// MCP servers: launch under runCtx so wallclock cancellation also tears
	// them down. Failures here abort the run before any API call. We close
	// handles in the deferred path below regardless of how the run exits,
	// so a panic or budget abort still cleans up subprocesses.
	//
	// Apply the same template-var substitution shell tasks get to each
	// MCP server's command args. Lets HCL like
	//   mcp "fs" { command = ["npx", "...", "${path}/data"] }
	// resolve to the task's working directory at launch time.
	mcps := make([]MCP, len(task.Agent.MCPs))
	for i, m := range task.Agent.MCPs {
		mcps[i] = m
		mcps[i].Command = make([]string, len(m.Command))
		for j, c := range m.Command {
			mcps[i].Command[j] = r.Replace(c)
		}
	}
	mcpHandles, mcpTools, mcpErr := LaunchMCPServers(runCtx, mcps, resolvedEnv, toolWriter)
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
		slog.String("run_id", task.RunID),
		slog.String("schedule", task.ScheduleName),
		slog.String("task", task.Name),
		slog.String("model", res.Model),
		slog.Int("input_tokens", res.InputTokens),
		slog.Int("output_tokens", res.OutputTokens),
		slog.Int("cache_read", res.CacheReadIn),
		slog.Int("cache_write", res.CacheWriteIn),
		slog.Float64("cost_usd", res.CostUSD),
		slog.Int64("duration_ms", durationMs),
		slog.String("stop_reason", res.StopReason),
		slog.String("response", res.Stdout),
		// Mirror the resolved prompt/system/tools/mcp_commands from the
		// start event onto the terminal event. The non-streaming pretty
		// renderer (renderAgentRun) consumes only the terminal event, so
		// without these duplicates it would lose the resolved-prompt
		// header line; for Loki/JSONL consumers it's a small redundancy
		// that keeps the run summary self-contained.
		slog.String("prompt", cfg.Prompt),
		slog.String("system", system),
		slog.Any("tools", resolvedTools),
		slog.Any("mcp_commands", resolvedMCPCommands),
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
			"run_id", task.RunID,
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
		"run_id", task.RunID,
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
