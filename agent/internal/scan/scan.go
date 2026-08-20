// Package scan implements security scanning on the agent: baseline checks,
// intrusion-trace detection, and vulnerability scanning. It executes a series
// of shell commands, collects findings, and streams progress back to the
// control server via ScanProgress messages.
package scan

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"watchman/proto/agentpb"
)

// Sender pushes agent->server messages.
type Sender interface {
	Send(msg *agentpb.AgentMessage) bool
}

// Manager handles scan requests.
type Manager struct {
	mu     sync.Mutex
	sender Sender
	log    *slog.Logger
}

// NewManager creates a scan manager.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log}
}

// SetSender wires the manager to the live connection hub.
func (m *Manager) SetSender(s Sender) {
	m.mu.Lock()
	m.sender = s
	m.mu.Unlock()
}

// Finding is a single security finding from a scan.
type Finding struct {
	Category   string `json:"category"`   // e.g. "ssh", "firewall", "process", "package"
	Severity   string `json:"severity"`   // critical / high / medium / low / info
	Title      string `json:"title"`      // short description
	Detail     string `json:"detail"`     // extended description
	Suggestion string `json:"suggestion"`  // fix recommendation (may include commands)
}

// Handle processes a ScanRequest from the server.
func (m *Manager) Handle(req *agentpb.ScanRequest) {
	go m.run(req)
}

func (m *Manager) run(req *agentpb.ScanRequest) {
	scanID := req.GetScanId()
	scanType := req.GetType() // baseline / intrusion / vuln

	m.log.Info("scan started", "scan_id", scanID, "type", scanType)

	// Send initial progress.
	m.sendProgress(scanID, 0.05, false, nil, "")

	// For Windows, we run a reduced scan; full checks target Linux.
	var findings []Finding
	if runtime.GOOS == "windows" {
		findings = m.scanWindows(req)
	} else {
		switch scanType {
		case "baseline":
			findings = m.scanBaseline(req)
		case "intrusion":
			findings = m.scanIntrusion(req)
		case "vuln":
			findings = m.scanVuln(req)
		default:
			findings = m.scanBaseline(req)
		}
	}

	m.sendProgress(scanID, 0.9, false, nil, "")

	findingsJSON, _ := json.Marshal(findings)

	// Send final progress with findings.
	m.sendProgress(scanID, 1.0, true, findingsJSON, "")

	m.log.Info("scan completed", "scan_id", scanID, "type", scanType, "findings", len(findings))
}

func (m *Manager) sendProgress(scanID string, progress float64, done bool, findingsJSON []byte, reportRef string) {
	m.mu.Lock()
	s := m.sender
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_ScanProgress{
			ScanProgress: &agentpb.ScanProgress{
				ScanId:       scanID,
				Progress:     progress,
				Done:         done,
				FindingsJson: findingsJSON,
				ReportRef:    reportRef,
			},
		},
	})
}

// runCmd executes a command with a timeout and returns trimmed stdout.
func runCmd(timeout time.Duration, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runBash runs a bash script with a timeout and returns trimmed output.
func runBash(timeout time.Duration, script string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func containsAny(s string, subs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	return runBash(2*time.Second, "test -f "+shellQuote(path)+" && echo yes") == "yes"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// addIf appends a finding if cond is true.
func addIf(findings *[]Finding, cond bool, category, severity, title, detail, suggestion string) {
	if cond {
		*findings = append(*findings, Finding{category, severity, title, detail, suggestion})
	}
}