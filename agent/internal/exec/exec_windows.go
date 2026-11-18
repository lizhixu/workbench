//go:build windows

package exec

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// configureProcessGroup puts the command in its own process group so a
// timeout kill can reach the whole tree it spawns (shell plus children),
// not just the shell itself.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

// killProcessGroup kills the command's process tree. Windows has no
// kill(-pgid) equivalent, so taskkill /T walks the tree. The process may
// already be gone when the timeout fires; that is not an error.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	out, err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).CombinedOutput()
	if err != nil {
		if bytes.Contains(out, []byte("not found")) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
