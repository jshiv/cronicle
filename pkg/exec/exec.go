package exec

import (
	"bytes"
	"errors"
	"os"
	goexec "os/exec"
	"syscall"
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
	// cmd := goexec.Command("/bin/bash", "-c", command)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.Error = err
		return result
	}
	stderr, err := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		result.Error = err
		return result
	}

	bb := bytes.NewBuffer([]byte{})
	_, err = bb.ReadFrom(stdout)
	result.Stdout = bb.String()

	be := bytes.NewBuffer([]byte{})
	_, err = be.ReadFrom(stderr)
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
