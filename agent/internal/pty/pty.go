// Package pty provides a cross-platform pseudo-terminal abstraction.
//
// The interface is implemented per-OS via build tags:
//   - linux/darwin: github.com/creack/pty
//   - windows:      ConPTY (github.com/UserExistsError/conpty)
package pty

// PTY is the abstract pseudo-terminal used by the shell session manager.
type PTY interface {
	// Read reads output from the terminal master.
	Read(p []byte) (int, error)
	// Write writes input to the terminal master.
	Write(p []byte) (int, error)
	// Resize changes the terminal window size (cols x rows).
	Resize(cols, rows uint16) error
	// Close releases the PTY and its child process.
	Close() error
}

// Config describes how to start a terminal session.
type Config struct {
	Shell   string // command to run (e.g. "bash", "powershell.exe")
	Cwd     string // working directory (empty = inherit)
	Cols    uint16
	Rows    uint16
	Account string // optional account name (MVP: ignored, runs as current user)
}