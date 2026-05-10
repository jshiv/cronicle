//go:build windows

package exec

import (
	goexec "os/exec"
)

// configureProcessGroup is a no-op on Windows: there's no POSIX process
// group to set up. goexec's default Cancel calls cmd.Process.Kill on the
// leader, which is the best we can do without spawning a Windows Job
// Object — a heavier piece of machinery we'd add only if we found
// cronicle running on Windows in production with stuck child trees.
func configureProcessGroup(cmd *goexec.Cmd) {}
