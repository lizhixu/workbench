//go:build linux

package uninstall

import (
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// Well-known install paths from the official Linux installer.
const (
	systemdUnit = "/etc/systemd/system/watchman-agent.service"
	sysvinitSvc = "/etc/init.d/watchman-agent"
	stdBinPath  = "/usr/local/bin/watchman-agent"
)

// removeService disables and removes the agent's service registration.
// self is the agent's own resolved executable path: a unit file is only
// touched when it references a watchman-agent binary, so an unrelated
// service can never be stopped by mistake.
//
// IMPORTANT: we must NOT "systemctl stop" (or "service ... stop")
// ourselves. The stop signal (SIGTERM) would terminate this very
// process before cleanup finishes — and once we ignore SIGTERM to
// survive it, a stop would instead block until systemd's TimeoutStopSec
// expires and then SIGKILL us. Disabling + removing the unit +
// daemon-reload is sufficient: with the unit file gone, systemd cannot
// restart us (Restart=always included) after we exit on our own at the
// end of the uninstall.
func (e *Executor) removeService(self string, _ bool) {
	// Survive our own service teardown: any stop path sends SIGTERM
	// to this process, and main() exits on that signal.
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM)

	if data, err := os.ReadFile(systemdUnit); err == nil {
		if !strings.Contains(string(data), "watchman-agent") {
			e.log.Warn("uninstall: systemd unit does not look like ours, skipping", "unit", systemdUnit)
		} else {
			e.runCmd("systemctl", "disable", "watchman-agent")
			if err := os.Remove(systemdUnit); err != nil && !os.IsNotExist(err) {
				e.log.Warn("uninstall: remove systemd unit failed", "err", err)
			} else {
				e.log.Info("uninstall: removed systemd unit")
			}
			e.runCmd("systemctl", "daemon-reload")
		}
	} else if !os.IsNotExist(err) {
		e.log.Warn("uninstall: read systemd unit failed", "err", err)
	}

	if data, err := os.ReadFile(sysvinitSvc); err == nil {
		if strings.Contains(string(data), "watchman-agent") {
			// No "service watchman-agent stop": that SIGTERMs this
			// process; we exit ourselves after cleanup instead.
			if err := os.Remove(sysvinitSvc); err != nil && !os.IsNotExist(err) {
				e.log.Warn("uninstall: remove sysvinit script failed", "err", err)
			} else {
				e.log.Info("uninstall: removed sysvinit script")
			}
		}
	}
}

func (e *Executor) runCmd(name string, args ...string) {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		e.log.Warn("uninstall: command failed", "cmd", name+" "+strings.Join(args, " "), "err", err, "out", strings.TrimSpace(string(out)))
	}
}
