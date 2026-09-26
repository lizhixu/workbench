// Package upgrade implements agent self-upgrade.
//
// When the control server sends an UpgradeRequest, the agent downloads the new
// binary from the provided URL, verifies its sha256, replaces its own
// executable, and restarts the service (systemd on Linux, scheduled task on
// Windows). Progress is streamed back as UpgradeProgress messages.
package upgrade

import (
	"crypto/ed25519"
	"crypto/hmac"
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
	// pubKey pins the Ed25519 public key allowed to sign upgrades.
	// Nil means "sha256 only" (not recommended for production).
	pubKey ed25519.PublicKey
}

// NewManager creates an upgrade manager.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log}
}

// SetPublicKeyHEX pins an Ed25519 public key (hex-encoded, 32 bytes) for
// upgrade signature verification. Empty string disables pinning.
func (m *Manager) SetPublicKeyHEX(hexKey string) error {
	if hexKey == "" {
		m.pubKey = nil
		return nil
	}
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return fmt.Errorf("decode upgrade public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("upgrade public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	m.pubKey = ed25519.PublicKey(raw)
	return nil
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

	// 2. Verify sha256 (required: refuse to install an unverified binary).
	if req.GetSha256() == "" {
		m.progress("error", 0, "upgrade rejected: server did not provide a sha256 checksum")
		return
	}
	m.progress("verifying", 0, "")
	actual, err := fileSha256(tmpPath)
	if err != nil {
		m.progress("error", 0, "hash: "+err.Error())
		return
	}
	if !hmac.Equal([]byte(actual), []byte(req.GetSha256())) {
		m.progress("error", 0, fmt.Sprintf("sha256 mismatch: expected %s got %s", req.GetSha256(), actual))
		return
	}
	m.progress("verifying", 1, "")

	// 3. Verify Ed25519 signature when the agent is configured with a
	//    pinned upgrade public key. The signed payload is
	//    "<version>\n<sha256>".
	if m.pubKey != nil {
		if len(req.GetSignature()) == 0 {
			m.progress("error", 0, "upgrade rejected: missing signature (agent pins an upgrade public key)")
			return
		}
		payload := []byte(req.GetVersion() + "\n" + req.GetSha256())
		if !ed25519.Verify(m.pubKey, payload, req.GetSignature()) {
			m.progress("error", 0, "upgrade rejected: invalid signature")
			return
		}
		m.log.Info("upgrade signature verified", "version", req.GetVersion())
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
	// On Windows, we can't overwrite a running exe directly; rename self first.
	if runtime.GOOS == "windows" {
		old := self + ".old"
		_ = os.Remove(old)
		if err := os.Rename(self, old); err != nil {
			return fmt.Errorf("rename old: %w", err)
		}
		if err := copyFile(newBin, self); err != nil {
			// Try to restore.
			_ = os.Rename(old, self)
			return fmt.Errorf("copy new: %w", err)
		}
		_ = os.Remove(old)
	} else {
		if err := copyFile(newBin, self); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		_ = os.Chmod(self, 0o755)
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