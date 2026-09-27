//go:build windows

package uninstall

import (
	"os/exec"
	"strings"
)

// removeService deletes the agent's Scheduled Task registration.
//
// The official installer registers the agent as the Scheduled Task
// "WatchmanAgent" (NOT a Windows service), so sc.exe is the wrong tool
// here. schtasks /Delete removes the task definition while the running
// instance keeps going; we exit ourselves afterwards, so there is
// nothing left for the task's restart policy to resurrect.
func (e *Executor) removeService(self string, removeData bool) {
	e.runCmd("schtasks", "/Delete", "/TN", "WatchmanAgent", "/F")
	// Best-effort fallback for a manually registered service, if any.
	// No "sc.exe stop": that would kill this process mid-uninstall.
	e.runCmd("sc.exe", "delete", "watchman-agent")

	// Schedule self + data dir deletion after exit: a running image
	// cannot be removed on Windows. The child cmd survives our exit.
	if self != "" {
		batch := "ping -n 3 127.0.0.1 >nul & del /f /q \"" + self + "\""
		if removeData && safeDataDir(e.stateDir) {
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
