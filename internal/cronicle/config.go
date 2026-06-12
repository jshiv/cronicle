package cronicle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Config is the configuration structure for the cronicle checker.
// https://raw.githubusercontent.com/mitchellh/golicense/master/config/config.go
// TODO: Add Version string `hcl:"version,optional"`
type Config struct {
	// Repo repository containing version controlled cronicle.hcl
	Repo *Repo `hcl:"repo,block"`
	// Heartbeat is the cron expression for the runner's "I'm alive"
	// signal. Originally this also controlled HCL reload cadence; that
	// duty has moved to ConfigRefresh so observers can keep a slow
	// heartbeat (e.g. 5m) while config reloads sub-second.
	// Default: "@every 5m" (set in cron.go's StartCron when empty).
	// Override in HCL with `heartbeat = "@every 30s"` for snappy
	// local-dev observability.
	Heartbeat string `hcl:"heartbeat,optional"`
	// ConfigRefresh is the cron expression for polling the config
	// source (file / http / s3 / postgres) for new content. Decoupled
	// from Heartbeat so the visual builder feels snappy without
	// flooding the heartbeat log. Default: "@every 1s" (set in cron.go's
	// StartCron when empty).
	ConfigRefresh string `hcl:"config_refresh,optional"`
	// Repos points at external dependent repos that maintain their own schedules remotly.
	Repos []string `hcl:"repos,optional"`
	// Timezone Location to run cron in. i.e. "America/New_York" [IANA Time Zone database]
	// https://en.wikipedia.org/wiki/List_of_tz_database_time_zones
	Timezone string `hcl:"timezone,optional"`
	// GitRemote *GitRemote `hcl:"git,block"`
	Queue     *Queue     `hcl:"queue,block"`
	Schedules []Schedule `hcl:"schedule,block"`
}

// Schedule is the configuration structure that defines a cron job consisting of tasks.
type Schedule struct {
	// Cron is the schedule interval. The field accepts standard cron
	// and other configurations listed here https://godoc.org/gopkg.in/robfig/cron.v2
	// i.e. ["@hourly", "@every 1h30m", "0 30 * * * *", "TZ=Asia/Tokyo 30 04 * * * *"]
	Name string `hcl:"name,label"`
	Cron string `hcl:"cron,optional"`
	// Timezone Location to run cron in. i.e. "America/New_York" [IANA Time Zone database]
	// https://en.wikipedia.org/wiki/List_of_tz_database_time_zones
	Timezone  string `hcl:"timezone,optional"`
	StartDate string `hcl:"start_date,optional"`
	EndDate   string `hcl:"end_date,optional"`
	Repo      *Repo  `hcl:"repo,block"`
	Tasks     []Task `hcl:"task,block"`
	// AgentTasks is the first-class schedule-level `agent "X" { ... }` HCL
	// block — folded into Tasks immediately after parse (see parse.go's
	// ParseFile). It's a parse-time intermediate and never serialized:
	// downstream consumers (DAG, projection, JSON wire) only see Tasks.
	AgentTasks []AgentTask `hcl:"agent,block" json:"-"`
	//Now is the execution time of the given schedule that will be used to
	//fill variable task command ${datetime}. The cron scheduler generally provides
	//the value.
	Now time.Time
	//repo given at the config level, will be overridden by repo given at schedule or task level.
	CronicleRepo *Repo
	//RunID is a per-trigger ULID that threads through every event emitted
	//for this fire of the schedule. Set by ProduceSchedule (cron tick),
	//the listener's trigger handler (HTTP), and ExecTasks (one-shot exec).
	//Used by the state plane projection to group events into a single run.
	RunID string
	//Source records why this fire happened — "cron", "http", "exec" — and
	//is included in schedule_start so the projection can populate
	//runs.source. Default empty (back-compat); ProduceSchedule fills it.
	Source string
	// RunCtx is the per-run cancel context used to preempt in-flight
	// task execution when a /v1/runs/{id}/cancel signal arrives. Set
	// by the HTTP worker before calling ExecuteTasks; nil in default
	// in-process mode (where cancellation isn't supported because the
	// run is foreground anyway). Excluded from JSON so the wire
	// payload stays clean.
	RunCtx context.Context `json:"-"`
}

// Task is the configuration structure that defines a task (i.e., a command)
type Task struct {
	Name              string   `hcl:"name,label"`
	Command           []string `hcl:"command,optional"`
	Depends           []string `hcl:"depends,optional"`
	ContinueOnFailure bool     `hcl:"continue_on_failure,optional"`
	Repo              *Repo    `hcl:"repo,block"`
	Retry             *Retry   `hcl:"retry,block"`
	Agent             *Agent   `hcl:"agent,block"`
	Env               []string `hcl:"env,optional"`
	Path              string
	CronicleRepo      *Repo
	CroniclePath      string
	Git               Git
	ScheduleName      string
	// RunID is propagated from the parent Schedule at execution time
	// (set in dag.go's ExecuteTasks). Threaded into per-task events so
	// the projection groups them under the right run.
	RunID string
	// RunCtx is the per-run cancel context propagated from
	// Schedule.RunCtx. The agent dispatch path uses it as parent so a
	// /v1/runs/{id}/cancel signal preempts long multi-turn runs at the
	// next ctx.Done() check between turns.
	RunCtx context.Context `json:"-"`

	// per-run state populated by Exec, read by Log. Unexported so they don't
	// pollute JSON marshaling or HCL encoding.
	lastTranscript string
	lastDurationMs int64
	shellStreamed  bool
	// ScratchDir is the schedule-scoped shared dir all tasks in one
	// schedule run can read/write. Set by Schedule.ExecuteTasks before
	// walking the DAG; substituted into prompts/commands as `${scratch}`.
	// Survives the run for transcript/audit access.
	ScratchDir string
	// LastRun is the start time of this schedule's previous SUCCESSFUL
	// run. Resolved by the dispatcher (resolveLastRun) against the
	// authoritative state store before ExecuteTasks runs — the producer
	// at enqueue, or the in-process ConsumeSchedule / ExecTasks paths.
	// ExecuteTasks never reads state for it (it also runs on distributed
	// workers, whose store has no run history). Substituted
	// into commands/prompts as `${last_run}` (RFC3339) and
	// `${last_run_epoch}` (unix seconds) so a task can fetch only what's
	// new "since the prior run" instead of guessing a lookback window.
	// Zero on a schedule's first-ever run (or with no state backend) —
	// both tokens then resolve to the empty string, which a script
	// should treat as "no prior run / full backfill".
	LastRun time.Time
}

// Agent is the configuration structure that defines an LLM agent invocation.
// When a task has an Agent block instead of a Command, the task is dispatched
// to pkg/agent rather than exec'd as a shell process.
type Agent struct {
	// Prompt is the user message sent to the model. ${date}/${datetime}/
	// ${timestamp}/${path} are substituted at execution time. Optional when
	// Skills is non-empty (the skill metadata can stand in for a literal
	// prompt by describing the work).
	Prompt string `hcl:"prompt,optional"`
	// Model selects the Claude model id, e.g. "claude-opus-4-7". Empty uses
	// the pkg/agent default.
	Model string `hcl:"model,optional"`
	// System is an optional system prompt.
	System string `hcl:"system,optional"`
	// MaxTokens caps the response length. 0 uses the pkg/agent default.
	MaxTokens int `hcl:"max_tokens,optional"`
	// BudgetUSD aborts the task if the run's cumulative cost exceeds this.
	// Checked after each turn for mid-run abort. 0 disables.
	BudgetUSD float64 `hcl:"budget_usd,optional"`
	// Tools is the allowlist of Anthropic-defined tools the agent can
	// invoke. When empty/omitted, defaults to all native tools (bash,
	// text_editor, web_search, web_fetch). Set explicitly to narrow the
	// universe. Unknown names cause Validate to fail.
	Tools []string `hcl:"tools,optional"`
	// Skills lists paths to SKILL.md files (relative to task.Path) that
	// describe Anthropic Agent Skills available for this run. Frontmatter
	// (name+description) of each skill is injected into the system prompt;
	// the body loads on demand via the load_skill tool (progressive
	// disclosure, per the standard).
	Skills []string `hcl:"skills,optional"`
	// MCPs lists Model Context Protocol servers to spawn for this agent
	// run. Each server is launched as a subprocess; cronicle speaks
	// JSON-RPC over its stdin/stdout, lists its tools, and registers
	// them with the agent (namespaced as `<server-name>__<tool-name>` so
	// names from different servers don't collide). Servers shut down
	// when the run ends.
	MCPs []MCP `hcl:"mcp,block"`
	// MaxTurns is the hard cap on agent loop iterations. 0 means default
	// (1 if no tools, 30 otherwise).
	MaxTurns int `hcl:"max_turns,optional"`
	// Wallclock is a duration string (e.g. "10m") setting an upper bound
	// on the entire run. Empty means default ("10m"). Parsed via
	// time.ParseDuration.
	Wallclock string `hcl:"wallclock,optional"`
}

// AgentTask is the first-class schedule-level `agent "name" { ... }` block.
// It's a parse-time convenience: every AgentTask is normalized to a regular
// Task (with the Task.Agent field populated) right after HCL decode, so the
// scheduler, DAG, queue, and projection only ever see Tasks.
//
// Why this exists: cronicle's product positioning ("scheduling for AI agents")
// reads best when the HCL surface matches — operators write
//
//	schedule "X" {
//	  agent "summarize" {
//	    prompt = "..."
//	    model  = "claude-opus-4-7"
//	  }
//	}
//
// instead of the legacy nested form
//
//	schedule "X" {
//	  task "summarize" {
//	    agent { prompt = "...", model = "..." }
//	  }
//	}
//
// Both forms parse identically (and both round-trip through Validate); the
// new shape is purely sugar. Existing configs keep working unchanged.
//
// Fields are the union of Task's task-shared properties (name, depends, repo,
// retry, env) and Agent's agent-specific properties (prompt, model, system,
// tools, skills, etc.) flattened to the top level. ToTask builds the equivalent
// Task; Schedule.normalizeAgentTasks appends them to Schedule.Tasks.
type AgentTask struct {
	Name    string   `hcl:"name,label"`
	Depends []string `hcl:"depends,optional"`
	Repo    *Repo    `hcl:"repo,block"`
	Retry   *Retry   `hcl:"retry,block"`
	Env     []string `hcl:"env,optional"`

	// Flattened Agent fields — identical semantics to the Agent struct.
	Prompt    string   `hcl:"prompt,optional"`
	Model     string   `hcl:"model,optional"`
	System    string   `hcl:"system,optional"`
	MaxTokens int      `hcl:"max_tokens,optional"`
	BudgetUSD float64  `hcl:"budget_usd,optional"`
	Tools     []string `hcl:"tools,optional"`
	Skills    []string `hcl:"skills,optional"`
	MCPs      []MCP    `hcl:"mcp,block"`
	MaxTurns  int      `hcl:"max_turns,optional"`
	Wallclock string   `hcl:"wallclock,optional"`
}

// ToTask returns the equivalent Task struct, with .Agent populated from the
// flattened agent fields. Callers route the resulting Task into the regular
// scheduler/DAG/exec paths unchanged.
func (a AgentTask) ToTask() Task {
	return Task{
		Name:    a.Name,
		Depends: a.Depends,
		Repo:    a.Repo,
		Retry:   a.Retry,
		Env:     a.Env,
		Agent: &Agent{
			Prompt:    a.Prompt,
			Model:     a.Model,
			System:    a.System,
			MaxTokens: a.MaxTokens,
			BudgetUSD: a.BudgetUSD,
			Tools:     a.Tools,
			Skills:    a.Skills,
			MCPs:      a.MCPs,
			MaxTurns:  a.MaxTurns,
			Wallclock: a.Wallclock,
		},
	}
}

// MCP is a Model Context Protocol server definition. Cronicle launches the
// command as a subprocess and speaks JSON-RPC over its stdin/stdout to
// discover and invoke tools. Each MCP block on an agent contributes its
// tools to the agent's tool universe, namespaced by the block label
// (e.g. `mcp "github" {...}` exposes tools as `github__create_issue`).
type MCP struct {
	// Name labels this server in the namespace. Tools from this server
	// appear to the model as `<Name>__<tool-name>`.
	Name string `hcl:"name,label"`
	// Command is the argv used to spawn the server. The first element is
	// the executable, the rest are arguments. Required and non-empty.
	Command []string `hcl:"command"`
	// Env is the list of env-var names to forward from cronicle's process
	// to the server (e.g. ["GITHUB_TOKEN"]). Empty inherits nothing.
	Env []string `hcl:"env,optional"`
}

// Repo is the structure that defines a git repository
type Repo struct {
	// URL is the remote git repository, a local path to git repository
	URL string `hcl:"url,optional"`
	// DeployKey is the path to the rsa private key that enables pull access to a
	// private remote repository.
	DeployKey string `hcl:"key,optional"`
	// Username + Password together produce HTTP Basic auth for the
	// remote. Password supports the standard HCL eval context, so the
	// idiomatic shape is to interpolate from the process env:
	//
	//   repo {
	//     url      = "https://api.cronicle.dev/git/<org>/<repo>"
	//     password = "${env.CRONICLE_TOKEN}"
	//   }
	//
	// Username is optional and defaults to "x" — the conventional
	// placeholder for git's HTTP Basic challenge. Every modern host
	// (GitHub, GitLab, Gitea, cronicle-git) ignores the username and
	// authenticates on the token in the password slot.
	//
	// SECURITY (M2, deferred): when the password uses ${env.X} the value
	// resolves at gohcl decode (parse time), so the resolved token is
	// present in the in-memory *Repo and flows into schedule.JSON() at
	// queue-push time (cron.go: queue <- schedule.JSON()). In
	// distributed mode the jobs table in state.db then stores the JSON
	// payload at rest, and the producer→worker HTTP transport carries
	// it on the wire. Today this is operator-restricted (state.db is
	// chmod-protected; the HTTP API is bearer-token + intended TLS),
	// and no production log line prints schedule.JSON, so the practical
	// exposure is limited. The structural fix is to keep the literal
	// "$secret.X" reference in queue payloads and re-resolve on the
	// worker side via the SecretStore (which already plumbs through
	// state.Backend) — that's a larger refactor pending a follow-up.
	// Until then prefer $secret.X over ${env.X} for repo passwords in
	// any distributed deployment; the $secret.X form is resolved at
	// task.Execute time, after the queue pop, so it never lands in the
	// jobs table or transport.
	Username string `hcl:"username,optional"`
	Password string `hcl:"password,optional"`
	// Branch and Commit are mutually exclusive. ErrBranchAndCommitGiven
	// rejects HCL that sets both. Historical bug: these fields had their
	// HCL tags crossed (`branch` populated Commit and vice versa) — fixed
	// here so the struct field name matches the HCL key.
	Branch string `hcl:"branch,optional"`
	Commit string `hcl:"commit,optional"`
	// KnownHosts is an optional path to an SSH known_hosts file used to
	// verify the remote's host key on a deploy-key (SSH) clone. Layered
	// on top of ~/.ssh/known_hosts, the CRONICLE_KNOWN_HOSTS env file,
	// and cronicle's embedded keys for the major public git hosts
	// (github.com, gitlab.com, bitbucket.org, ssh.dev.azure.com). When a
	// host is in none of these sources the clone is refused unless
	// AcceptNewHostKey is set (or the CRONICLE_SSH_NO_VERIFY escape hatch
	// is used). Ignored for HTTPS clones.
	KnownHosts string `hcl:"known_hosts,optional"`
	// AcceptNewHostKey, when true, accepts an unknown host's key on a
	// deploy-key clone instead of refusing it — the SSH equivalent of
	// OpenSSH's StrictHostKeyChecking=accept-new, scoped to this repo.
	// A key that MISMATCHES a known one is still always refused (that's
	// the MITM signal). Does not persist the accepted key. Use for
	// self-hosted git hosts not in the embedded set; prefer pinning via
	// KnownHosts when you can.
	AcceptNewHostKey bool `hcl:"accept_new_host_key,optional"`
}

// Retry defines the retry count and delay in number and seconds.
type Retry struct {
	//Count: Number of retry attemts to make after first attempt
	Count int `hcl:"count,optional"`
	//Seconds: Number of seconds to wait between retry attempts
	Seconds int `hcl:"seconds,optional"`
	//Minutes: Number of minutes to wait between retry attempts
	Minutes int `hcl:"minutes,optional"`
	//Hours: Number of hours to wait between retry attempts
	Hours int `hcl:"hours,optional"`
}

// Queue is a vestigial HCL block, kept so existing cronicle.hcl files
// that still declare `queue { ... }` continue to parse. Its fields
// have no effect: as of v0.5 queue mode is derived from whether the
// HTTP listener is configured (see `cronicle run`'s --listen flag).
// Future queue tuning (retention windows, claim visibility) may layer
// in here, which is why we keep the block rather than reject it.
type Queue struct {
	Type string `hcl:"type,optional"`
	Addr string `hcl:"addr,optional"`
}

var (
	//ErrBranchAndCommitGiven is thrown because commit and branch are mutually exclusive to identify a repo
	ErrBranchAndCommitGiven = errors.New("branch and commit can not both be populated")
	//ErrRepoNotGiven is thrown because a git repo is not given, for the case where Checkout or other git
	//specific methods are called
	ErrRepoNotGiven = errors.New("git repo has not been given")
	//ErrIfRepoGivenAndPathNotGiven is thrown because a repo was given but the path to the local repo has not been provided
	ErrIfRepoGivenAndPathNotGiven = errors.New("if repo is populated, path must also be given at runtime")
	//ErrScheduleNameEmpty is thrown because schedule.Name == "", hcl can not be given with schedule "" {}
	ErrScheduleNameEmpty = errors.New("schedule name can not be an empty string")
	//ErrTaskNameEmpty is thrown because task.Name == "", hcl can not be given with task "" {}
	ErrTaskNameEmpty = errors.New("task name can not be an empty string")
	//ErrRepoGivenAndURLNotGiven is thrown because task.Name == "", hcl can not be given with task "" {}
	ErrRepoGivenAndURLNotGiven = errors.New("if repo is populated, it must have an assoicated url")
	//ErrCommandAndAgentBothGiven is thrown when a task has both a command and an agent block.
	//A task is exec'd as one or the other, not both.
	ErrCommandAndAgentBothGiven = errors.New("task can not have both command and agent")
	//ErrAgentNeedsPromptOrSkills is thrown when an agent block has neither a
	//prompt nor any skills configured. At least one is required so the agent
	//has direction.
	ErrAgentNeedsPromptOrSkills = errors.New("agent block requires prompt or skills")
)

// Validate validates the fields and sets the default values.
func (task *Task) Validate() error {
	if task.Repo != nil {
		if task.Repo.Branch != "" && task.Repo.Commit != "" {
			return ErrBranchAndCommitGiven
		}
	}

	if task.Repo != nil {
		if task.Repo.URL == "" {
			return ErrRepoGivenAndURLNotGiven
		}
	}

	if task.Repo != nil {
		if task.Path == "" {
			return ErrIfRepoGivenAndPathNotGiven
		}
	}

	if task.Agent != nil {
		if len(task.Command) > 0 {
			return ErrCommandAndAgentBothGiven
		}
		if task.Agent.Prompt == "" && len(task.Agent.Skills) == 0 {
			return ErrAgentNeedsPromptOrSkills
		}
		for _, name := range task.Agent.Tools {
			if !knownAgentTool(name) {
				return fmt.Errorf("unknown agent tool %q (known: bash, text_editor, web_search, web_fetch)", name)
			}
		}
		// Skill paths are workspace-relative; reject absolute paths and
		// `..` traversal at config time so a misconfigured task fails
		// before any API calls are made.
		for _, sp := range task.Agent.Skills {
			if sp == "" {
				return errors.New("agent skill path must not be empty")
			}
			if filepath.IsAbs(sp) {
				return fmt.Errorf("agent skill path %q must be relative to task workspace", sp)
			}
			cleaned := filepath.Clean(sp)
			if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
				return fmt.Errorf("agent skill path %q escapes task workspace", sp)
			}
		}
		// MCP servers: each block needs a non-empty name and command. The
		// name has to be a valid tool-name component since we namespace
		// tools as `<name>__<tool>` and Anthropic enforces ^[a-zA-Z0-9_-]{1,128}$.
		for i, m := range task.Agent.MCPs {
			if m.Name == "" {
				return fmt.Errorf("agent mcp[%d]: name (block label) is required", i)
			}
			if !isValidToolNamePart(m.Name) {
				return fmt.Errorf("agent mcp %q: name must match ^[a-zA-Z0-9_-]+$", m.Name)
			}
			if len(m.Command) == 0 {
				return fmt.Errorf("agent mcp %q: command is required", m.Name)
			}
		}
		if task.Agent.MaxTurns < 0 {
			return errors.New("agent max_turns must be >= 0")
		}
		if task.Agent.Wallclock != "" {
			if _, err := time.ParseDuration(task.Agent.Wallclock); err != nil {
				return fmt.Errorf("agent wallclock parse: %w", err)
			}
		}
	}

	return nil
}

// isValidToolNamePart reports whether s is a legal MCP server label. Used
// to namespace MCP tools as `<server>__<tool>` — Anthropic's tool name
// regex is ^[a-zA-Z0-9_-]{1,128}$ and we want the prefix to fit cleanly.
func isValidToolNamePart(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// knownAgentTool reports whether name is a recognized agent tool in the
// current cronicle build.
//
// Client-side (cronicle implements execution): bash, text_editor, git.
// Server-side (Anthropic runs them; we just declare): web_search, web_fetch.
func knownAgentTool(name string) bool {
	switch name {
	case "bash", "text_editor", "git", "web_search", "web_fetch":
		return true
	}
	return false
}

// Validate checks that schedule.Name is not empty and assigns task.ScheduleName
// on a whole config struct.
func (conf *Config) Validate() error {

	if conf.Timezone != "" {
		if _, err := time.LoadLocation(conf.Timezone); err != nil {
			return err
		}
	}

	scheduleNameCount := make(map[string]int)
	for _, schedule := range conf.Schedules {
		if schedule.Timezone != "" {
			if _, err := time.LoadLocation(schedule.Timezone); err != nil {
				return err
			}
		}

		if schedule.Name == "" {
			return ErrScheduleNameEmpty
		}

		for _, task := range schedule.Tasks {
			if task.Name == "" {
				return ErrTaskNameEmpty
			}
		}
		scheduleNameCount[schedule.Name]++
	}

	for k, v := range scheduleNameCount {
		if v > 1 {
			return fmt.Errorf(`schedule "%s" {} is listed %v times, please change the name`, k, v)
		}
	}
	return nil
}

// PropigateTaskProperties pushes schedule.Name, schedule.Repo and the repo path down to the task values.
// It also populates task.Git.ReferenceName with task.Branch or HEAD.
func (conf *Config) PropigateTaskProperties(croniclePath string) {
	for i := range conf.Schedules {
		if conf.Schedules[i].Timezone == "" {
			conf.Schedules[i].Timezone = conf.Timezone
		}
		conf.Schedules[i].CronicleRepo = conf.Repo
		conf.Schedules[i].PropigateTaskProperties(croniclePath)
	}
}

// PropigateTaskProperties pushes schedule.Name, schedule.Repo and the repo path down to the task values.
// It also populates task.Git.ReferenceName with task.Branch or HEAD.
func (schedule *Schedule) PropigateTaskProperties(croniclePath string) {
	// Assign the path for each task or schedule repo
	for i, task := range schedule.Tasks {
		// if task.Branch != "" {
		// 	task.Git.ReferenceName = plumbing.NewBranchReferenceName(task.Branch)
		// } else {
		// 	task.Git.ReferenceName = plumbing.HEAD
		// }

		var path string
		var taskPath string
		var repo *Repo

		// If the task is associated to a repo
		if task.Repo != nil {
			repo = task.Repo
			// Internally propigate REPO properties from schedule.Repo if they are defined in schedle but not in task
			if schedule.Repo != nil {
				if task.Repo.URL == "" && schedule.Repo.URL != "" {
					repo.URL = schedule.Repo.URL
				}
				if task.Repo.DeployKey == "" && schedule.Repo.DeployKey != "" {
					repo.DeployKey = schedule.Repo.DeployKey
				}
				if task.Repo.Branch == "" && schedule.Repo.Branch != "" {
					repo.Branch = schedule.Repo.Branch
				}
				if task.Repo.Commit == "" && schedule.Repo.Commit != "" {
					repo.Commit = schedule.Repo.Commit
				}
			}
			// If a Schedule is associated to a repo, all sub tasks are by default associated
		} else if schedule.Repo != nil {
			repo = schedule.Repo
			// Else the repo is the cronicle repo
		} else {
			repo = nil
		}
		// If the task is associated to a repo, put it in the repos directory
		if task.Repo != nil {
			path, _ = LocalRepoDir(croniclePath, task.Repo.URL)
			// If a Schedule is associated to a repo, all sub tasks are by default associated
		} else if schedule.Repo != nil {
			path, _ = LocalRepoDir(croniclePath, schedule.Repo.URL)
			// Else the path is the root croniclePath
		} else {
			path = croniclePath
		}

		// If the given task is associatated to a repo, clone the task to an independent path
		if repo != nil {
			taskPath = filepath.Join(path, schedule.Name, task.Name)
			// Else the task is associated to the root croniclePath
		} else {
			taskPath = croniclePath
		}

		schedule.Tasks[i].Path = taskPath
		schedule.Tasks[i].CroniclePath = croniclePath
		schedule.Tasks[i].CronicleRepo = schedule.CronicleRepo
		schedule.Tasks[i].Repo = repo
		schedule.Tasks[i].ScheduleName = schedule.Name
	}
}

// Default returns a basic default Config
// it includes a single schedule that runs every 5 seconds
// and a single "Hello World" task.
func Default() Config {

	var task Task
	task.Name = "bar"
	task.Command = []string{"/bin/echo", "Hello World --date=${date}"}

	var schedule Schedule
	schedule.Name = "foo"
	schedule.Cron = "@every 5s"
	schedule.Tasks = []Task{task}

	var conf Config
	conf.Heartbeat = "@every 5m"
	conf.Schedules = []Schedule{schedule}

	return conf
}

// TaskArray is an array of Task structs,
// calling config.TaskArray() ensures that each task.ScheduleName is filled
type TaskArray []Task

// TaskArray exports a TaskArray all tasks in a given config,
// additionally, it ensures that task.ScheduleName is propigated
func (conf *Config) TaskArray() TaskArray {

	err := conf.Validate() // ensure that schedule.Name and task.ScheduleName are not empty
	if err != nil {
		Fatal(err)
	}
	tasks := TaskArray{}

	for _, schedule := range conf.Schedules {
		for _, task := range schedule.Tasks {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

// ScheduleMap is an map of key=schedul.Name: value=Schedule struct,
type ScheduleMap map[string]Schedule

// ScheduleMap exports a ScheduleMap all schedules in a given config
func (conf *Config) ScheduleMap() ScheduleMap {

	scheduleMap := ScheduleMap{}
	for _, schedule := range conf.Schedules {
		scheduleMap[schedule.Name] = schedule
	}
	return scheduleMap
}

// TaskMap is an map of key=task.Name: value=Task struct,
type TaskMap map[string]Task

// TaskMap exports a TaskMap all tasks in a given config,
func (schedule *Schedule) TaskMap() TaskMap {

	taskMap := TaskMap{}
	for _, task := range schedule.Tasks {
		taskMap[task.Name] = task
	}
	return taskMap
}

// FilterTasks returns a task array where
// only matching task.Name = taskName and schedule.Name=scheduleName
// if taskName = "" and scheduleName = "" then all tasks will be returned
// empty strings are intrepreted as no filtering requested.
func (t TaskArray) FilterTasks(taskName string, scheduleName string) TaskArray {

	tasks := TaskArray{}
	if taskName == "" && scheduleName != "" {
		// if taskName is "", retrun all tasks with a matching scheduleName
		for _, task := range t {
			if task.ScheduleName == scheduleName {
				tasks = append(tasks, task)
			}
		}
	} else if taskName != "" && scheduleName == "" {
		// if scheduleName is "", retrun all tasks with a matching taskName
		for _, task := range t {
			if task.Name == taskName {
				tasks = append(tasks, task)
			}
		}
	} else if taskName != "" && scheduleName != "" {
		// if taskName and scheduleName are both gicen, return only tasks that match on both
		for _, task := range t {
			if task.Name == taskName && task.ScheduleName == scheduleName {
				tasks = append(tasks, task)
			}
		}
	} else {
		//if both arguments are "", return all tasks
		tasks = t
	}

	return tasks
}
