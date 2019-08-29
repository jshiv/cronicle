package bash

import (
	"bytes"
	"os/exec"
	"syscall"

	log "github.com/sirupsen/logrus"
)

//BashRun pulls from examples at https://zaiste.net/executing_external_commands_in_go/
// and //https://gist.github.com/mchirico/6045501

type Data struct {
	Command    string
	Stdout     string
	Stderr     string
	ExitStatus int
}

func Bash(command string) Data {
	var data Data
	data.Command = command
	cmd := exec.Command("/bin/bash", "-c", command)
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
	data.Stdout = bb.String()

	be := bytes.NewBuffer([]byte{})
	_, err = be.ReadFrom(stderr)
	data.Stderr = be.String()

	var waitStatus syscall.WaitStatus
	if err := cmd.Wait(); err != nil {
		if err != nil {
			log.Fatal(err)
		}
		if exitError, ok := err.(*exec.ExitError); ok {
			waitStatus = exitError.Sys().(syscall.WaitStatus)
			data.ExitStatus = waitStatus.ExitStatus()
		}
	} else {
		// Success
		waitStatus = cmd.ProcessState.Sys().(syscall.WaitStatus)
		data.ExitStatus = waitStatus.ExitStatus()
	}
	return data
}

func LogStdout(data Data) {
	log.SetFormatter(&log.TextFormatter{
		DisableColors: false,
		FullTimestamp: true,
	})
	log.WithFields(log.Fields{
		"bash": data.Command,
	}).Info(data.Stdout)
}
