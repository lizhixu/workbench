//go:build linux

package uninstall

import (
	"os"
	"os/exec"
	"strings"
)

// Well-known install paths from the official Linux installer.
const (
	systemdUnit = "/etc/systemd/system/watchman-agent.service"
	sysvinitSvc = "/etc/init.d/watchman-agent"
	stdBinPath  = "/usr/local/bin/watchman-agent"
)

// removeService stops and removes the agent's service registration.
// self is the agent's own resolved executable path: a unit file is only
// touched when it references a watchman-agent binary, so an unrelated
// service can never be stopped by mistake.
func (e *Executor) removeService(self string) {
	if data, err := os.ReadFile(systemdUnit); err == nil {
		if !strings.Contains(string(data), "watchman-agent") {
			e.log.Warn("uninstall: systemd unit does not look like ours, skipping", "unit", systemdUnit)
		} else {
			e.runCmd("systemctl", "stop", "watchman-agent")
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
			e.runCmd("service", "watchman-agent", "stop")
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
