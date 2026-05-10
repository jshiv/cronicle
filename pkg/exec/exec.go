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

//BashRun pulls from examples at https://zaiste.net/executing_external_commands_in_go/
// and //https://gist.github.com/mchirico/6045501

type Result struct {
	Command    []string
	Stdout     string
	Stderr     string
	ExitStatus int
	Error      error
}

//Execute executes a given command []string at dir path and returns the results as a Result struct.
//TODO: Add method for running command that does not collect stdout, just writes to stdout
// in order to handle complex/verbose logging
func Execute(command []string, dir string, env []string) Result {
	var result Result
	result.Command = command
	var cmd *goexec.Cmd
	switch len(command) {
	case 1:
		cmd = goexec.Command(command[0])
	default:
		cmd = goexec.Command(command[0], command[1:]...)
	}
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for _, e := range env {
		cmd.Env = append(cmd.Env, e)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.Error = err
		return result
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		result.Error = err
		return result
	}
	if err := cmd.Start(); err != nil {
		result.Error = err
		return result
	}

	bb := bytes.NewBuffer([]byte{})
	if _, err := bb.ReadFrom(stdout); err != nil {
		result.Error = err
	}
	result.Stdout = bb.String()

	be := bytes.NewBuffer([]byte{})
	if _, err := be.ReadFrom(stderr); err != nil && result.Error == nil {
		result.Error = err
	}
	result.Stderr = be.String()

	var waitStatus syscall.WaitStatus
	if err := cmd.Wait(); err != nil {
		if exitError, ok := err.(*goexec.ExitError); ok {
			waitStatus = exitError.Sys().(syscall.WaitStatus)
			result.ExitStatus = waitStatus.ExitStatus()
			result.Error = errors.New(exitError.Error())
		}
	} else {
		// Success
		waitStatus = cmd.ProcessState.Sys().(syscall.WaitStatus)
		result.ExitStatus = waitStatus.ExitStatus()
	}

	if result.Error == nil {
		result.Error = exitStatusError(result)
	}
	return result
}

// ExecuteWithStreamContext is like ExecuteWithStream but ties the child
// process lifetime to ctx: when ctx is cancelled (e.g. via the agent
// runtime's wallclock deadline), the entire process group is signalled with
// SIGTERM, escalating to SIGKILL after 2 seconds if needed. The child runs
// in its own pgid so subprocesses spawned by the command (think
// `bash -c "sleep 30 | cat"`) all die together — important for unattended
// cronicle agents where a stuck bash invocation would otherwise outlive
// the run.
func ExecuteWithStreamContext(ctx context.Context, command []string, dir string, env []string, stdoutW, stderrW io.Writer) Result {
	var result Result
	result.Command = command
	var cmd *goexec.Cmd
	switch len(command) {
	case 1:
		cmd = goexec.CommandContext(ctx, command[0])
	default:
		cmd = goexec.CommandContext(ctx, command[0], command[1:]...)
	}
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for _, e := range env {
		cmd.Env = append(cmd.Env, e)
	}
	// Process-group handling is platform-specific. On unix, the child runs in
	// its own pgid so cancellation signals the whole group (catches `bash -c
	// "sleep 30 | cat"` style sub-fans). On windows, the goexec default
	// (cmd.Process.Kill) is the best we have without spawning a Job Object.
	configureProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	var stdoutBuf, stderrBuf bytes.Buffer
	if stdoutW != nil {
		cmd.Stdout = io.MultiWriter(&stdoutBuf, stdoutW)
	} else {
		cmd.Stdout = &stdoutBuf
	}
	if stderrW != nil {
		cmd.Stderr = io.MultiWriter(&stderrBuf, stderrW)
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

// ExecuteWithStream is like Execute but pipes process stdout and stderr
// through stdoutW / stderrW in real time while still capturing the full
// output into the returned Result. Pass nil writers to skip streaming for
// either stream — Execute is implemented as ExecuteWithStream(..., nil, nil).
//
// Used by the cronicle pretty-mode dispatch path so a long-running shell
// task's output appears at the terminal as the process emits it, while the
// final Result still carries the complete stdout/stderr for transcripts and
// the structured log event.
func ExecuteWithStream(command []string, dir string, env []string, stdoutW, stderrW io.Writer) Result {
	var result Result
	result.Command = command
	var cmd *goexec.Cmd
	switch len(command) {
	case 1:
		cmd = goexec.Command(command[0])
	default:
		cmd = goexec.Command(command[0], command[1:]...)
	}
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for _, e := range env {
		cmd.Env = append(cmd.Env, e)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if stdoutW != nil {
		cmd.Stdout = io.MultiWriter(&stdoutBuf, stdoutW)
	} else {
		cmd.Stdout = &stdoutBuf
	}
	if stderrW != nil {
		cmd.Stderr = io.MultiWriter(&stderrBuf, stderrW)
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

func exitStatusError(res Result) error {
	var err error
	switch res.ExitStatus {
	case 0:
		err = nil
	case 1:
		err = errors.New(res.Stderr)
	default:
		err = errors.New(res.Stderr)
	}
	return err
}
