//go:build !windows

package exec

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the command in its own process group so a
// timeout kill can reach the whole tree it spawns (shell plus children),
// not just the shell itself.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the command's process group. It is the exec
// package's Cancel function, invoked when the command's context expires.
// SIGKILL cannot be caught or ignored, so even a child that traps SIGTERM
// dies with the group. A negative pid addresses the group (whose id is the
// shell's pid, per Setpgid).
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
