package exec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	goexec "os/exec"
	"syscall"
	"time"
)

// Result captures everything the caller cares about from a command run.
// Stdout/Stderr are the full captured bytes (also streamed live when the
// caller passes writers). ExitStatus is the platform exit code; Error
// is non-nil when ExitStatus != 0 or when the run itself failed to start.
type Result struct {
	Command    []string
	Stdout     string
	Stderr     string
	ExitStatus int
	Error      error
}

// runOptions captures the dial-knobs that vary across the public entry
// points so the one real implementation (run) can be parameterized
// without four near-identical copies.
type runOptions struct {
	ctx          context.Context
	dir          string
	env          []string
	stdoutW      io.Writer
	stderrW      io.Writer
	useProcGroup bool // when true, child runs in its own pgid so cancellation kills its subprocesses
	// scrubEnv selects the env policy:
	//   false (default) — child inherits the entire parent env (os.Environ())
	//                     plus opts.env. Long-standing behavior; callers that
	//                     run user-defined commands (shell tasks) rely on it.
	//   true            — child inherits only a small benign allowlist (PATH,
	//                     HOME, USER, LANG, LC_ALL, TZ) plus opts.env. Used
	//                     by the bash tool so an agent-emitted `env`/`set`
	//                     command can't read worker-process state.
	scrubEnv bool
}

// scrubbedInheritKeys is the env var allowlist that ExecuteScrubbed*
// passes through from the parent process. Kept intentionally short:
// only the variables a typical interactive shell command needs to
// behave normally. Anything else must come in via opts.env.
//
// Notably absent: ANTHROPIC_API_KEY, GITHUB_TOKEN, CRONICLE_* — the
// whole point of scrubbed mode is to NOT leak these into agent-
// controlled subprocesses. cronicle's secretstore dispenses these
// per-task instead, gated by what the task's HCL env array declares.
var scrubbedInheritKeys = []string{
	"PATH",
	"HOME",
	"USER",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TZ",
	"SHELL",
}

// run is the canonical implementation. All four public entry points
// (Execute, ExecuteContext, ExecuteWithStream, ExecuteWithStreamContext)
// thread through here. Keeping the orchestration in one function avoids
// the previous drift where Execute didn't honor ctx but
// ExecuteWithStreamContext did — a subtle bug for callers that wanted
// "non-streaming, but cancellable."
func run(command []string, opts runOptions) Result {
	var result Result
	result.Command = command

	ctx := opts.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	var cmd *goexec.Cmd
	switch len(command) {
	case 0:
		result.Error = errors.New("exec: empty command")
		return result
	case 1:
		cmd = goexec.CommandContext(ctx, command[0])
	default:
		cmd = goexec.CommandContext(ctx, command[0], command[1:]...)
	}
	cmd.Dir = opts.dir
	if opts.scrubEnv {
		// Scrubbed mode: only the allowlisted parent vars pass through.
		base := make([]string, 0, len(scrubbedInheritKeys)+len(opts.env))
		for _, k := range scrubbedInheritKeys {
			if v, ok := os.LookupEnv(k); ok {
				base = append(base, k+"="+v)
			}
		}
		cmd.Env = append(base, opts.env...)
	} else {
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, opts.env...)
	}

	if opts.useProcGroup {
		configureProcessGroup(cmd)
		// 2 seconds after ctx fires we escalate from SIGTERM (which
		// CommandContext sends by default) to SIGKILL via WaitDelay.
		// Enough room for a well-behaved process to drain stdio, short
		// enough that "cancel" feels responsive.
		cmd.WaitDelay = 2 * time.Second
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if opts.stdoutW != nil {
		cmd.Stdout = io.MultiWriter(&stdoutBuf, opts.stdoutW)
	} else {
		cmd.Stdout = &stdoutBuf
	}
	if opts.stderrW != nil {
		cmd.Stderr = io.MultiWriter(&stderrBuf, opts.stderrW)
	} else {
		cmd.Stderr = &stderrBuf
	}

	runErr := cmd.Run()
	result.Stdout = stdoutBuf.String()
	result.Stderr = stderrBuf.String()

	var waitStatus syscall.WaitStatus
	if runErr != nil {
		if exitError, ok := runErr.(*goexec.ExitError); ok {
			waitStatus = exitError.Sys().(syscall.WaitStatus)
			result.ExitStatus = waitStatus.ExitStatus()
			result.Error = errors.New(exitError.Error())
		} else {
			result.Error = runErr
		}
	} else if cmd.ProcessState != nil {
		waitStatus = cmd.ProcessState.Sys().(syscall.WaitStatus)
		result.ExitStatus = waitStatus.ExitStatus()
	}

	if result.Error == nil {
		result.Error = exitStatusError(result)
	}
	return result
}

// Execute runs command at dir with env and returns its captured output.
// Equivalent to ExecuteContext(context.Background(), ...). Kept for
// callers that don't care about cancellation (one-shot cronicle exec,
// tests).
func Execute(command []string, dir string, env []string) Result {
	return run(command, runOptions{dir: dir, env: env})
}

// ExecuteContext is Execute that honors ctx — when ctx is canceled (e.g.
// a /v1/runs/{id}/cancel signal routed via the worker's SSE channel),
// the child process is killed via SIGTERM, escalating to SIGKILL after
// a 2-second drain window. The child runs in its own process group so
// `bash -c "sleep 30 | cat"` style sub-fans die together.
//
// This is the entry point cronicle's shell-task dispatch uses when the
// task has a per-run RunCtx set (HTTP-worker mode). Default in-process
// mode passes context.Background() since the run is foreground and
// cancel would not be meaningful.
func ExecuteContext(ctx context.Context, command []string, dir string, env []string) Result {
	return run(command, runOptions{
		ctx:          ctx,
		dir:          dir,
		env:          env,
		useProcGroup: true,
	})
}

// ExecuteWithStream is like Execute but streams stdout/stderr through
// the given writers in real time (the returned Result still carries
// the full captured output). Used by the cronicle pretty-mode dispatch
// path so long shell tasks appear at the terminal incrementally.
func ExecuteWithStream(command []string, dir string, env []string, stdoutW, stderrW io.Writer) Result {
	return run(command, runOptions{
		dir:     dir,
		env:     env,
		stdoutW: stdoutW,
		stderrW: stderrW,
	})
}

// ExecuteWithStreamContext is ExecuteWithStream that honors ctx —
// streaming the live output while still killing the child (and its
// subprocesses) on cancellation. The agent's bash tool uses this so a
// long bash invocation inside a multi-turn agent run dies when the
// run's context expires (wallclock budget or operator cancel).
func ExecuteWithStreamContext(ctx context.Context, command []string, dir string, env []string, stdoutW, stderrW io.Writer) Result {
	return run(command, runOptions{
		ctx:          ctx,
		dir:          dir,
		env:          env,
		stdoutW:      stdoutW,
		stderrW:      stderrW,
		useProcGroup: true,
	})
}

// ExecuteScrubbedStreamContext is ExecuteWithStreamContext but with a
// scrubbed parent env: the child inherits only the PATH/HOME/USER/
// LANG/LC_ALL/LC_CTYPE/TZ/SHELL allowlist from os.Environ. Everything
// else comes from env. Use this for subprocesses driven by untrusted
// or semi-trusted callers — the agent's bash tool, future MCP-driven
// shells — so the worker-process env (which may have been hardened
// against secret leakage, but may still carry CRONICLE_* deployment
// metadata) doesn't leak through.
func ExecuteScrubbedStreamContext(ctx context.Context, command []string, dir string, env []string, stdoutW, stderrW io.Writer) Result {
	return run(command, runOptions{
		ctx:          ctx,
		dir:          dir,
		env:          env,
		stdoutW:      stdoutW,
		stderrW:      stderrW,
		useProcGroup: true,
		scrubEnv:     true,
	})
}

func exitStatusError(res Result) error {
	switch res.ExitStatus {
	case 0:
		return nil
	default:
		return errors.New(res.Stderr)
	}
}
