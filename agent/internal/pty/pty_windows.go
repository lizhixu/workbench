//go:build windows

// Package pty windows implementation using ConPTY.
package pty

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/UserExistsError/conpty"
)

type windowsPTY struct {
	c  *conpty.ConPty
	ex *exec.Cmd
}

func Open(cfg Config) (PTY, error) {
	shell := cfg.Shell
	if shell == "" {
		// Prefer PowerShell, fall back to cmd.
		if p, err := exec.LookPath("powershell.exe"); err == nil {
			shell = p
		} else {
			shell = "cmd.exe"
		}
	}

	// conpty requires a working directory; default to the agent's cwd.
	cwd := cfg.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// Build the command line. For built-in shells use the full path.
	var commandLine string
	if filepath.IsAbs(shell) {
		commandLine = shell
	} else {
		// Let Windows resolve it via the system path by using the name as-is
		// in CreateProcess; conpty accepts a command line string.
		commandLine = shell
	}

	c, err := conpty.Start(commandLine, conpty.ConPtyDimensions(int(cfg.Cols), int(cfg.Rows)), conpty.ConPtyWorkDir(cwd))
	if err != nil {
		// Fallback: try with an explicit path resolution / cmd.exe.
		if strings.HasSuffix(strings.ToLower(shell), "powershell.exe") {
			c, err = conpty.Start("powershell.exe", conpty.ConPtyDimensions(int(cfg.Cols), int(cfg.Rows)), conpty.ConPtyWorkDir(cwd))
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	return &windowsPTY{c: c}, nil
}

func (w *windowsPTY) Read(p []byte) (int, error) {
	return w.c.Read(p)
}

func (w *windowsPTY) Write(p []byte) (int, error) {
	return w.c.Write(p)
}

func (w *windowsPTY) Resize(cols, rows uint16) error {
	return w.c.Resize(int(cols), int(rows))
}

func (w *windowsPTY) Close() error {
	w.c.Close()
	return nil
}