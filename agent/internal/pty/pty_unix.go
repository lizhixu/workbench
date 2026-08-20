//go:build !windows

// Package pty unix implementation using github.com/creack/pty.
package pty

import (
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type unixPTY struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

func Open(cfg Config) (PTY, error) {
	shell := cfg.Shell
	if shell == "" {
		shell = "bash"
	}

	cmd := exec.Command(shell)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	cmd.Env = os.Environ()

	// Start the command in a pseudo-terminal.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: cfg.Cols,
		Rows: cfg.Rows,
	})
	if err != nil {
		return nil, err
	}

	return &unixPTY{ptmx: ptmx, cmd: cmd}, nil
}

func (u *unixPTY) Read(p []byte) (int, error) {
	return u.ptmx.Read(p)
}

func (u *unixPTY) Write(p []byte) (int, error) {
	return u.ptmx.Write(p)
}

func (u *unixPTY) Resize(cols, rows uint16) error {
	return pty.Setsize(u.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

func (u *unixPTY) Close() error {
	_ = u.ptmx.Close()
	// Kill the child process group and reap it without blocking the caller.
	// A shell that's waiting for input won't exit on SIGTERM alone, so escalate
	// to SIGKILL. Wait in a goroutine so a stubborn child can't stall the agent's
	// gRPC recv loop (which would back-pressure the server's sendPump and cause
	// every subsequent op to time out).
	if u.cmd.Process != nil {
		_ = syscall.Kill(-u.cmd.Process.Pid, syscall.SIGTERM)
		go func() {
			// Give the process a brief grace period, then force-kill.
			timer := time.AfterFunc(2*time.Second, func() {
				_ = syscall.Kill(-u.cmd.Process.Pid, syscall.SIGKILL)
			})
			_ = u.cmd.Wait()
			timer.Stop()
		}()
	}
	return nil
}