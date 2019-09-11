package bash

import (
	"bytes"
	"os/exec"
	"syscall"

	log "github.com/sirupsen/logrus"
)

//BashRun pulls from examples at https://zaiste.net/executing_external_commands_in_go/
// and //https://gist.github.com/mchirico/6045501

type Result struct {
	Command    []string
	Stdout     string
	Stderr     string
	ExitStatus int
}

func Bash(command []string) Result {
	var result Result
	result.Command = command
	var cmd *exec.Cmd
	switch len(command) {
	case 1:
		cmd = exec.Command(command[0])
	default:
		cmd = exec.Command(command[0], command[1:]...)
	}
	// cmd := exec.Command("/bin/bash", "-c", command)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	bb := bytes.NewBuffer([]byte{})
	_, err = bb.ReadFrom(stdout)
	result.Stdout = bb.String()

	be := bytes.NewBuffer([]byte{})
	_, err = be.ReadFrom(stderr)
	result.Stderr = be.String()

	var waitStatus syscall.WaitStatus
	if err := cmd.Wait(); err != nil {
		if err != nil {
			log.Fatal(err)
		}
		if exitError, ok := err.(*exec.ExitError); ok {
			waitStatus = exitError.Sys().(syscall.WaitStatus)
			result.ExitStatus = waitStatus.ExitStatus()
		}
	} else {
		// Success
		waitStatus = cmd.ProcessState.Sys().(syscall.WaitStatus)
		result.ExitStatus = waitStatus.ExitStatus()
	}
	return result
}

func LogStdout(result Result) {
	log.WithFields(log.Fields{
		"bash": result.Command,
	}).Info(result.Stdout)
}
