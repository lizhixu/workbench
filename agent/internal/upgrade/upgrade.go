// Package upgrade implements agent self-upgrade.
//
// When the control server sends an UpgradeRequest, the agent downloads the new
// binary from the provided URL, verifies its sha256, replaces its own
// executable, and restarts the service (systemd on Linux, scheduled task on
// Windows). Progress is streamed back as UpgradeProgress messages.
package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"watchman/proto/agentpb"
)

// Sender pushes messages to the control server.
type Sender interface {
	Send(msg *agentpb.AgentMessage) bool
}

// Manager handles a single upgrade at a time.
type Manager struct {
	log    *slog.Logger
	sender Sender
	busy   bool
}

// NewManager creates an upgrade manager.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log}
}

// SetSender wires the live connection.
func (m *Manager) SetSender(s Sender) {
	m.sender = s
}

// Handle processes an UpgradeRequest from the server.
func (m *Manager) Handle(req *agentpb.UpgradeRequest) {
	if m.busy {
		m.progress("error", 0, "upgrade already in progress")
		return
	}
	m.busy = true
	defer func() { m.busy = false }()

	go m.doUpgrade(req)
}

func (m *Manager) doUpgrade(req *agentpb.UpgradeRequest) {
	m.log.Info("upgrade starting", "version", req.GetVersion(), "url", req.GetUrl())

	// 1. Download
	m.progress("downloading", 0, "")
	tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("watchman-agent-new-%d", time.Now().UnixNano()))
	if err := m.download(req.GetUrl(), tmpPath, req.GetSha256()); err != nil {
		m.progress("error", 0, "download: "+err.Error())
		return
	}
	m.progress("downloading", 1, "")

	// 2. Verify sha256
	if req.GetSha256() != "" {
		m.progress("verifying", 0, "")
		actual, err := fileSha256(tmpPath)
		if err != nil {
			m.progress("error", 0, "hash: "+err.Error())
			return
		}
		if actual != req.GetSha256() {
			m.progress("error", 0, fmt.Sprintf("sha256 mismatch: expected %s got %s", req.GetSha256(), actual))
			return
		}
		m.progress("verifying", 1, "")
	}

	// 3. Replace self
	m.progress("replacing", 0, "")
	if err := m.replaceSelf(tmpPath); err != nil {
		m.progress("error", 0, "replace: "+err.Error())
		return
	}
	m.progress("replacing", 1, "")

	// 4. Restart
	m.progress("restarting", 0, "")
	m.log.Info("upgrade complete, restarting", "version", req.GetVersion())
	m.progress("restarting", 1, "")

	// Trigger restart in a separate goroutine so the progress message can flush.
	go func() {
		time.Sleep(1 * time.Second)
		m.restart()
	}()
}

func (m *Manager) download(url, dest, expectedSha string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func fileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (m *Manager) replaceSelf(newBin string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	selfDir := filepath.Dir(self)
	staging := filepath.Join(selfDir, fmt.Sprintf(".%s.new-%d", filepath.Base(self), time.Now().UnixNano()))

	// 1. Copy new binary to staging file in the same directory (ensures same filesystem).
	if err := copyFile(newBin, staging); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("stage new binary: %w", err)
	}
	_ = os.Chmod(staging, 0o755)

	// 2. On Windows, a running executable cannot be unlinked or overwritten in place;
	// rename the running executable first, then replace.
	if runtime.GOOS == "windows" {
		old := self + ".old"
		_ = os.Remove(old)
		if err := os.Rename(self, old); err != nil {
			_ = os.Remove(staging)
			return fmt.Errorf("rename running binary: %w", err)
		}
		if err := os.Rename(staging, self); err != nil {
			// Try rollback.
			_ = os.Rename(old, self)
			_ = os.Remove(staging)
			return fmt.Errorf("replace binary: %w", err)
		}
		_ = os.Remove(old)
	} else {
		// On Linux/Unix, unlink or atomic rename over a running text-busy executable
		// is fully supported by the kernel, while in-place truncation open() triggers ETXTBSY.
		if err := os.Rename(staging, self); err != nil {
			_ = os.Remove(staging)
			return fmt.Errorf("atomic rename replace: %w", err)
		}
	}

	_ = os.Remove(newBin)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func (m *Manager) restart() {
	// The agent is typically run as a systemd service (Linux) or scheduled
	// task (Windows). We exit the process; the supervisor restarts us.
	m.log.Info("exiting for restart")
	os.Exit(0)
}

func (m *Manager) progress(stage string, progress float64, errMsg string) {
	if m.sender == nil {
		return
	}
	m.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_UpgradeProgress{
			UpgradeProgress: &agentpb.UpgradeProgress{
				Stage:    stage,
				Progress: progress,
				Error:    errMsg,
			},
		},
	})
}