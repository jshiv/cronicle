package exec

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("exec", func() {

	It("Execute /bin/echo cronicle should echo cronicle on unix system", func() {
		command := []string{"/bin/echo", "cronicle"}
		res := Execute(command, "./", []string{})
		Expect(res.Error).To(BeNil())
		expected := Result{Command: command, Stdout: "cronicle\n", Stderr: "", ExitStatus: 0, Error: nil}
		Expect(res).To(Equal(expected))
	})
	It("Execute python3 os.environ.get('FOO') should print 'bar'", func() {
		command := []string{"python3", "-c", "import os; print(os.environ.get('FOO'))"}
		env := []string{"FOO=bar"}
		res := Execute(command, "./", env)
		Expect(res.Error).To(BeNil())
		expected := Result{Command: command, Stdout: "bar\n", Stderr: "", ExitStatus: 0, Error: nil}
		Expect(res).To(Equal(expected))
	})

	It("Execute no command should not execute, but return Error: `fork/exec : no such file or directory` ", func() {
		command := []string{""}
		res := Execute(command, "./", []string{})
		err := res.Error
		err.Error()
		// expected := Result{Command: command, Stdout: "", Stderr: "", ExitStatus: 0, Error: nil}
		Expect(err.Error()).To(Equal("exec: no command"))
	})

	It("Execute /bin/bash not_a_script should fail in execution ", func() {
		command := []string{"/bin/bash", "not_a_script"}
		res := Execute(command, "./", []string{})
		err := res.Error
		err.Error()

		exitError := errors.New("exit status 127")
		expected := Result{Command: command, Stdout: "", Stderr: "/bin/bash: not_a_script: No such file or directory\n", ExitStatus: 127, Error: exitError}
		Expect(res).To(Equal(expected))
		Expect(err).To(Equal(exitError))
	})

	It("ExecuteWithStreamContext should kill the process group when ctx is cancelled", func() {
		// Spawn `bash -c "sleep 30 | sleep 30"` so there are subprocesses
		// in the group, set a short timeout, and verify the call returns
		// well before the sleep would have completed.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		started := time.Now()
		res := ExecuteWithStreamContext(ctx, []string{"/bin/sh", "-c", "sleep 30 | sleep 30"},
			"./", nil, nil, nil)
		elapsed := time.Since(started)
		Expect(elapsed).To(BeNumerically("<", 5*time.Second))
		Expect(res.Error).NotTo(BeNil())
	})
})
