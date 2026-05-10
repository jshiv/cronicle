//go:build !windows

package exec

import (
	goexec "os/exec"
	"syscall"
)

// configureProcessGroup runs the child in its own process group and replaces
// goexec's default Cancel hook (which kills only the leader) with a SIGTERM
// to the entire group. Combined with the cmd.WaitDelay set by the caller,
// any non-exiting children get SIGKILL'd shortly after — important for
// shell pipelines and unattended agent runs where a stuck subprocess would
// otherwise outlive the wallclock deadline.
func configureProcessGroup(cmd *goexec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}
