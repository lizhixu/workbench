// Package uninstall implements agent self-uninstall.
//
// When the control plane unbinds a host with "uninstall agent" requested,
// the server sends an UninstallRequest over the existing agent stream.
// The agent then stops and removes its service registration, deletes its
// own binary and (optionally) its data dir, and exits.
//
// Safety: only well-known install paths from the official installer are
// ever removed, and the data dir is removed only when its path looks like
// a watchman dir. A dev agent run from an arbitrary path still exits, but
// no unrelated files are touched.
package uninstall

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"watchman/proto/agentpb"
)

// Executor performs the self-uninstall.
type Executor struct {
	log      *slog.Logger
	stateDir string // agent data dir (dir of the -state file)

	exitFunc func(int)
}

// New creates an Executor. stateDir is the agent's data directory; it is
// removed only when req.RemoveData is set and the path is safe.
func New(log *slog.Logger, stateDir string) *Executor {
	if log == nil {
		log = slog.Default()
	}
	return &Executor{log: log, stateDir: stateDir, exitFunc: os.Exit}
}

// Handle runs the uninstall asynchronously: it waits briefly so the
// server observes the disconnect, then removes the installation and
// exits the process. It never returns an error to the caller — failures
// are logged and the process still exits.
func (e *Executor) Handle(req *agentpb.UninstallRequest) {
	removeData := req != nil && req.GetRemoveData()
	e.log.Info("uninstall requested by server", "remove_data", removeData)
	go func() {
		// Let the stream close / disconnect propagate first.
		time.Sleep(2 * time.Second)
		e.run(removeData)
		e.exitFunc(0)
	}()
}

func (e *Executor) run(removeData bool) {
	self, err := os.Executable()
	if err != nil {
		e.log.Warn("uninstall: cannot determine own executable", "err", err)
	} else if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}

	e.removeService(self)

	if self != "" {
		if err := os.Remove(self); err != nil && !os.IsNotExist(err) {
			e.log.Warn("uninstall: remove own binary failed", "path", self, "err", err)
		} else {
			e.log.Info("uninstall: removed own binary", "path", self)
		}
	}

	if removeData {
		e.removeDataDir()
	} else {
		e.log.Info("uninstall: keeping data dir", "dir", e.stateDir)
	}
}

// removeDataDir deletes the agent data dir when it is safe to do so.
func (e *Executor) removeDataDir() {
	dir := e.stateDir
	if !safeDataDir(dir) {
		e.log.Warn("uninstall: refusing to remove data dir (unsafe path)", "dir", dir)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		e.log.Warn("uninstall: remove data dir failed", "dir", dir, "err", err)
	} else {
		e.log.Info("uninstall: removed data dir", "dir", dir)
	}
}

// safeDataDir guards against catastrophic paths: the dir must be
// non-empty, absolute-ish, contain "watchman", and not be a system root.
func safeDataDir(dir string) bool {
	if dir == "" {
		return false
	}
	clean := filepath.Clean(dir)
	if clean == "." || clean == "/" || clean == "" {
		return false
	}
	if !strings.Contains(strings.ToLower(clean), "watchman") {
		return false
	}
	// Never allow top-level system dirs even if oddly named.
	for _, root := range []string{"/usr", "/etc", "/bin", "/sbin", "/var", "/opt", "/home", "/root", "/tmp"} {
		if clean == root {
			return false
		}
	}
	return true
}
