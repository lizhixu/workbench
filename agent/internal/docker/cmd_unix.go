//go:build !windows

package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// dockerCommand builds a process-group isolated command whose entire process
// tree is killed when ctx expires, matching agent/internal/exec. This keeps
// timed-out CLI wrappers (such as podman-docker) or child helpers from
// lingering after timeout and holding storage locks or thrashing disk reads.
func dockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
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
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
