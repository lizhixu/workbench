// Package exec implements remote command execution on the agent.
//
// It runs a command (or multi-line script) via the OS shell and returns
// stdout/stderr/exit-code as an ExecResult.
//
// Three mechanisms keep a long-running command from going dark:
//
//   - Stall detection: if the command produces no stdout/stderr output for
//     stall_timeout_sec, the agent kills the whole process group and reports
//     "stalled: no output for <N>s". A silent-but-alive command (a pull
//     stuck on a dead network, a tar waiting on a hung mount) is killed
//     promptly instead of burning the whole wall-clock budget. Stall 0
//     disables it; the wall-clock timeout stays as the outer bound.
//   - Streaming: output is sent to the server in ~200ms / 4KB batches while
//     the command runs, so the operator watches progress live. Chunks are
//     also accumulated into the final ExecResult, which stays complete.
//   - Cancellation: the server can send ExecCancel; the agent kills the
//     process group and reports the result as "cancelled".
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
	"sync/atomic"
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
	regexp.MustCompile(`rm\s+-[rfRF]+\s+/(\s|$)`),           // rm -rf /
	regexp.MustCompile(`rm\s+-[rfRF]+\s+/\*`),               // rm -rf /*
	regexp.MustCompile(`mkfs\.`),                            // mkfs.<fs>
	regexp.MustCompile(`dd\s+if=.*of=/dev/(sd|nvme|hd|vd)`), // dd to disk
	regexp.MustCompile(`>\s*/dev/sd[a-z]`),                  // overwrite disk
	regexp.MustCompile(`:\s*\(\)\s*\{\s*:\|:\&\s*\}\s*;`),   // fork bomb
	regexp.MustCompile(`chmod\s+-R\s+777\s+/\s*$`),          // chmod -R 777 /
	regexp.MustCompile(`kill\s+-9\s+-1`),                    // kill all
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

// runningExec is one exec currently running on this agent. Cancel() can
// reach it through the manager's registry; run() owns the fields after
// the command's context exists.
type runningExec struct {
	mu sync.Mutex
	// cancel kills the command's context (and, via cmd.Cancel, the whole
	// process group). Set by run() once the context exists.
	cancel context.CancelFunc
	// cancelPending records a Cancel() that arrived before run() had
	// created the context; run() applies it immediately instead.
	cancelPending bool
	cancelled     atomic.Bool
}

// Manager handles exec requests.
type Manager struct {
	mu     sync.Mutex
	sender Sender
	log    *slog.Logger

	// running tracks live execs by exec_id so ExecCancel can reach them.
	// Guarded by runMu (separate from mu: sender is read on every result
	// send, the registry only churns per exec).
	runMu   sync.Mutex
	running map[string]*runningExec
}

// NewManager creates an exec manager.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, running: make(map[string]*runningExec)}
}

// SetSender wires the manager to the live connection hub.
func (m *Manager) SetSender(s Sender) {
	m.mu.Lock()
	m.sender = s
	m.mu.Unlock()
}

// Handle processes an ExecRequest from the server.
func (m *Manager) Handle(req *agentpb.ExecRequest) {
	entry := &runningExec{}
	m.runMu.Lock()
	m.running[req.GetExecId()] = entry
	m.runMu.Unlock()
	go m.run(req, entry)
}

// Cancel stops a running exec: it kills the whole process group and run()
// reports the result with error "cancelled". Unknown or already-finished
// execs are a no-op (returns false); a live exec returns true.
func (m *Manager) Cancel(req *agentpb.ExecCancel) bool {
	if req == nil {
		return false
	}
	m.runMu.Lock()
	entry, ok := m.running[req.GetExecId()]
	m.runMu.Unlock()
	if !ok {
		return false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.cancelled.Store(true)
	if entry.cancel != nil {
		entry.cancel()
	} else {
		// The command's context does not exist yet; run() applies this
		// as soon as it creates it.
		entry.cancelPending = true
	}
	return true
}

// chunkWriter accumulates one stream's output for the final ExecResult
// while flushing it to the server in ~200ms / 4KB batches. Batching keeps
// a chatty command (docker build, tar -v) from flooding the stream with
// one message per write, while the operator still watches progress live
// instead of staring at a spinner until the command ends.
//
// A chunk is always sent before the final ExecResult (run() flushes after
// cmd.Run returns), and all flushes for one stream run under the same
// mutex, so chunks for a stream arrive in order. Sending under the mutex
// can stall the command's writer for a few seconds if the stream is dead
// (Sender gives up after 5s); that is bounded and preferable to losing
// the ordering guarantee.
type chunkWriter struct {
	mu        sync.Mutex
	sender    Sender
	execID    string
	stream    string // "stdout" or "stderr"
	buf       bytes.Buffer
	pending   []byte
	lastFlush time.Time
	lastOut   *atomic.Int64 // shared with the stall watchdog; millis since epoch
}

func newChunkWriter(sender Sender, execID, stream string, lastOut *atomic.Int64) *chunkWriter {
	return &chunkWriter{sender: sender, execID: execID, stream: stream, lastOut: lastOut, lastFlush: time.Now()}
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, _ := w.buf.Write(p)
	w.lastOut.Store(time.Now().UnixMilli())
	w.pending = append(w.pending, p...)
	if len(w.pending) >= 4096 || time.Since(w.lastFlush) >= 200*time.Millisecond {
		w.flushLocked()
	}
	return n, nil
}

// flush sends any pending bytes as one ExecOutput chunk. Caller holds w.mu.
func (w *chunkWriter) flushLocked() {
	if len(w.pending) == 0 || w.sender == nil {
		return
	}
	chunk := w.pending
	w.pending = nil
	w.lastFlush = time.Now()
	w.sender.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_ExecOutput{
			ExecOutput: &agentpb.ExecOutput{ExecId: w.execID, Chunk: chunk, Stream: w.stream},
		},
	})
}

// flush drains any remaining bytes (called after the command ends, before
// the final ExecResult is sent, so chunks always precede the result).
func (w *chunkWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

// bytes returns a copy of the accumulated output. It takes the mutex so a
// copy goroutine still finishing its last write (WaitDelay path) cannot
// race the read.
func (w *chunkWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, w.buf.Len())
	copy(out, w.buf.Bytes())
	return out
}

func (m *Manager) run(req *agentpb.ExecRequest, entry *runningExec) {
	defer func() {
		m.runMu.Lock()
		delete(m.running, req.GetExecId())
		m.runMu.Unlock()
	}()

	// Snapshot the sender up front: streaming needs it from the first
	// output byte, not only when the result is ready.
	m.mu.Lock()
	sender := m.sender
	m.mu.Unlock()

	sendResult := func(result *agentpb.ExecResult) {
		if sender != nil {
			sender.Send(&agentpb.AgentMessage{
				Payload: &agentpb.AgentMessage_ExecResult{ExecResult: result},
			})
		}
	}

	// Local high-risk interception (last line of defense, independent of
	// server-side policy).
	if reason := checkLocalPolicy(req.GetCommand()); reason != "" {
		m.log.Warn("command blocked by local policy", "command", req.GetCommand(), "reason", reason)
		sendResult(&agentpb.ExecResult{
			ExecId:   req.GetExecId(),
			ExitCode: -1,
			Error:    reason,
		})
		return
	}

	timeout := time.Duration(req.GetTimeoutSec()) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	// runCtx is the command's context: the wall-clock timeout expires it,
	// and Cancel() expires it too. Both go through cmd.Cancel, which kills
	// the whole process group.
	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), timeout)
	defer cancelTimeout()
	runCtx, runCancel := context.WithCancel(timeoutCtx)
	defer runCancel()

	entry.mu.Lock()
	entry.cancel = runCancel
	if entry.cancelPending {
		// A cancel arrived before the context existed.
		entry.cancelled.Store(true)
		runCancel()
	}
	entry.mu.Unlock()

	shell := req.GetShell()
	if shell == "" {
		shell = defaultShell()
	}

	var cmd *exec.Cmd
	if req.GetIsScript() {
		// Run as a script piped to the shell.
		cmd = exec.CommandContext(runCtx, shell)
		cmd.Stdin = bytes.NewReader([]byte(req.GetCommand()))
	} else {
		cmd = exec.CommandContext(runCtx, shell, "-c", req.GetCommand())
		if runtime.GOOS == "windows" && (shell == "cmd" || shell == "cmd.exe") {
			cmd = exec.CommandContext(runCtx, shell, "/c", req.GetCommand())
		}
	}

	// A timeout must kill the whole process tree, not just the shell.
	// exec.CommandContext only kills the direct child: a timed-out
	// `docker run` left its docker CLI alive as an orphan, and the
	// orphan held the output pipes open so cmd.Run never returned —
	// the server gave up first while the orphan went on to create the
	// container. The shell runs in its own process group; Cancel kills
	// the group (SIGKILL on unix, taskkill /T on Windows). WaitDelay
	// bounds the pipe drain so a descendant that escaped the group
	// (setsid) still cannot stall the result beyond a few seconds.
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second

	// Stall detection: the command is alive only while it keeps talking.
	// lastOut is fed by both stream writers; the watchdog ticks once a
	// second and kills the group when the silence outlasts the budget.
	// stall 0 disables it (only the wall-clock timeout applies).
	// The stall flag is set only when the kill actually landed on a live
	// process: if the command happened to exit in the same instant, the
	// kill fails and no stall is reported.
	stallSec := req.GetStallTimeoutSec()
	var stalled atomic.Bool
	var lastOut atomic.Int64
	lastOut.Store(time.Now().UnixMilli())
	if stallSec > 0 {
		done := make(chan struct{})
		defer close(done)
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					if time.Now().UnixMilli()-lastOut.Load() > int64(stallSec)*1000 {
						if err := killProcessGroup(cmd); err == nil {
							stalled.Store(true)
						}
						return
					}
				}
			}
		}()
	}

	start := time.Now()
	outW := newChunkWriter(sender, req.GetExecId(), "stdout", &lastOut)
	errW := newChunkWriter(sender, req.GetExecId(), "stderr", &lastOut)
	cmd.Stdout = outW
	cmd.Stderr = errW

	err := cmd.Run()
	duration := time.Since(start)

	// Flush the last partial batch before the result, so the streamed
	// chunks always precede the ExecResult that carries the same output.
	outW.flush()
	errW.flush()

	result := &agentpb.ExecResult{
		ExecId:     req.GetExecId(),
		DurationMs: duration.Milliseconds(),
		Stdout:     outW.bytes(),
		Stderr:     errW.bytes(),
	}
	if err != nil {
		switch {
		case entry.cancelled.Load():
			// Server-initiated cancel (ExecCancel).
			result.Error = "cancelled"
			result.ExitCode = -1
		case stalled.Load():
			result.Error = fmt.Sprintf("stalled: no output for %ds", stallSec)
			result.ExitCode = -1
		case timeoutCtx.Err() == context.DeadlineExceeded:
			result.Error = fmt.Sprintf("timeout after %s", timeout)
			result.ExitCode = -1
		default:
			if exitErr, ok := err.(*exec.ExitError); ok {
				result.ExitCode = int32(exitErr.ExitCode())
			} else {
				result.Error = err.Error()
				result.ExitCode = -1
			}
		}
	} else {
		result.ExitCode = 0
	}

	sendResult(result)
}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "bash"
}
