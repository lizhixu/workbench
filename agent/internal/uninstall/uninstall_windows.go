//go:build windows

package uninstall

import (
	"os/exec"
	"strings"
)

// removeService stops and deletes the Windows service registration.
// The running executable cannot delete itself on Windows, so file
// removal is deferred to a detached cmd that runs after we exit.
func (e *Executor) removeService(self string) {
	if out, err := exec.Command("sc.exe", "query", "watchman-agent").CombinedOutput(); err != nil {
		e.log.Info("uninstall: no watchman-agent service", "out", strings.TrimSpace(string(out)))
	} else {
		e.runCmd("sc.exe", "stop", "watchman-agent")
		e.runCmd("sc.exe", "delete", "watchman-agent")
	}

	// Schedule self + data dir deletion after exit: a running image
	// cannot be removed on Windows. The child cmd survives our exit.
	if self != "" {
		batch := "ping -n 3 127.0.0.1 >nul & del /f /q \"" + self + "\""
		if safeDataDir(e.stateDir) {
			// Data dir is removed by the shared removeDataDir() too, but
			// files may be locked while we run; retry here after exit.
			batch += " & rmdir /s /q \"" + e.stateDir + "\""
		}
		cmd := exec.Command("cmd.exe", "/C", batch)
		if err := cmd.Start(); err != nil {
			e.log.Warn("uninstall: schedule self-delete failed", "err", err)
		} else {
			e.log.Info("uninstall: scheduled self-delete after exit")
		}
	}
}

func (e *Executor) runCmd(name string, args ...string) {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		e.log.Warn("uninstall: command failed", "cmd", name+" "+strings.Join(args, " "), "err", err, "out", strings.TrimSpace(string(out)))
	}
}
