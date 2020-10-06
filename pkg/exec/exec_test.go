package exec

import (
	"errors"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("exec", func() {

	It("Execute /bin/echo cronicle should echo cronicle on unix system", func() {
		command := []string{"/bin/echo", "cronicle"}
		res := Execute(command, "./")
		Expect(res.Error).To(BeNil())
		expected := Result{Command: command, Stdout: "cronicle\n", Stderr: "", ExitStatus: 0, Error: nil}
		Expect(res).To(Equal(expected))
	})

	It("Execute no command should not execute, but return Error: `fork/exec : no such file or directory` ", func() {
		command := []string{""}
		res := Execute(command, "./")
		err := res.Error
		err.Error()
		// expected := Result{Command: command, Stdout: "", Stderr: "", ExitStatus: 0, Error: nil}
		Expect(err.Error()).To(Equal("fork/exec : no such file or directory"))
	})

	It("Execute /bin/bash not_a_script should fail in execution ", func() {
		command := []string{"/bin/bash", "not_a_script"}
		res := Execute(command, "./")
		err := res.Error
		err.Error()

		exitError := errors.New("exit status 127")
		expected := Result{Command: command, Stdout: "", Stderr: "/bin/bash: not_a_script: No such file or directory\n", ExitStatus: 127, Error: exitError}
		Expect(res).To(Equal(expected))
		Expect(err).To(Equal(exitError))
	})
})
