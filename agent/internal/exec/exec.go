// Package exec implements remote command execution on the agent.
//
// It runs a command (or multi-line script) via the OS shell and returns
// stdout/stderr/exit-code as an ExecResult.
package exec

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"runtime"
	"sync"
	"time"

	"watchman/proto/agentpb"
)

// Sender pushes agent->server messages.
type Sender interface {
	Send(msg *agentpb.AgentMessage) bool
}

// localHighRiskPatterns are built-in high-risk command patterns enforced on
// the agent itself as a last line of defense, independent of server-side
// policy. These catch catastrophically destructive commands even if the
// control server policy is disabled or bypassed.
var localHighRiskPatterns = []*regexp.Regexp{
	regexp.MustCompile(`rm\s+-[rfRF]+\s+/(\s|$)`),         // rm -rf /
	regexp.MustCompile(`rm\s+-[rfRF]+\s+/\*`),             // rm -rf /*
	regexp.MustCompile(`mkfs\.`),                           // mkfs.<fs>
	regexp.MustCompile(`dd\s+if=.*of=/dev/(sd|nvme|hd|vd)`), // dd to disk
	regexp.MustCompile(`>\s*/dev/sd[a-z]`),                 // overwrite disk
	regexp.MustCompile(`:\s*\(\)\s*\{\s*:\|:\&\s*\}\s*;`), // fork bomb
	regexp.MustCompile(`chmod\s+-R\s+777\s+/\s*$`),        // chmod -R 777 /
	regexp.MustCompile(`kill\s+-9\s+-1`),                   // kill all
}

// checkLocalPolicy returns a non-empty reason string when the command matches
// a local high-risk pattern (command should be blocked).
func checkLocalPolicy(command string) string {
	for _, re := range localHighRiskPatterns {
		if re.MatchString(command) {
			return fmt.Sprintf("command blocked by local agent policy: matched %s", re.String())
		}
	}
	return ""
}

// Manager handles exec requests.
type Manager struct {
	mu     sync.Mutex
	sender Sender
	log    *slog.Logger
}

// NewManager creates an exec manager.
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

// Handle processes an ExecRequest from the server.
func (m *Manager) Handle(req *agentpb.ExecRequest) {
	go m.run(req)
}

func (m *Manager) run(req *agentpb.ExecRequest) {
	// Local high-risk interception (last line of defense, independent of
	// server-side policy).
	if reason := checkLocalPolicy(req.GetCommand()); reason != "" {
		m.log.Warn("command blocked by local policy", "command", req.GetCommand(), "reason", reason)
		result := &agentpb.ExecResult{
			ExecId:   req.GetExecId(),
			ExitCode: -1,
			Error:    reason,
		}
		m.mu.Lock()
		s := m.sender
		m.mu.Unlock()
		if s != nil {
			s.Send(&agentpb.AgentMessage{
				Payload: &agentpb.AgentMessage_ExecResult{ExecResult: result},
			})
		}
		return
	}

	timeout := time.Duration(req.GetTimeoutSec()) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	shell := req.GetShell()
	if shell == "" {
		shell = defaultShell()
	}

	var cmd *exec.Cmd
	if req.GetIsScript() {
		// Run as a script piped to the shell.
		cmd = exec.CommandContext(ctx, shell)
		cmd.Stdin = bytes.NewReader([]byte(req.GetCommand()))
	} else {
		cmd = exec.CommandContext(ctx, shell, "-c", req.GetCommand())
		if runtime.GOOS == "windows" && (shell == "cmd" || shell == "cmd.exe") {
			cmd = exec.CommandContext(ctx, shell, "/c", req.GetCommand())
		}
	}

	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	duration := time.Since(start)

	result := &agentpb.ExecResult{
		ExecId:     req.GetExecId(),
		DurationMs: duration.Milliseconds(),
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.Bytes(),
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result.Error = fmt.Sprintf("timeout after %s", timeout)
			result.ExitCode = -1
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = int32(exitErr.ExitCode())
		} else {
			result.Error = err.Error()
			result.ExitCode = -1
		}
	} else {
		result.ExitCode = 0
	}

	m.mu.Lock()
	s := m.sender
	m.mu.Unlock()
	if s != nil {
		s.Send(&agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_ExecResult{ExecResult: result},
		})
	}
}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "bash"
}