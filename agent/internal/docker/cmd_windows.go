//go:build windows

package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// dockerCommand mirrors the Unix process-group isolation on Windows so a
// timed-out docker CLI invocation terminates its entire child process tree.
func dockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if _, err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).CombinedOutput(); err != nil {
			// Avoid matching localized taskkill strings (e.g. zh-CN); probing
			// Process.Kill tells us in a locale-neutral way if it already exited.
			if killErr := cmd.Process.Kill(); errors.Is(killErr, os.ErrProcessDone) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
