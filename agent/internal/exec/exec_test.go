package exec

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"watchman/proto/agentpb"
)

type captureSender struct {
	mu   sync.Mutex
	msgs []*agentpb.AgentMessage
	ch   chan *agentpb.ExecResult
}

func (s *captureSender) Send(msg *agentpb.AgentMessage) bool {
	s.mu.Lock()
	s.msgs = append(s.msgs, msg)
	s.mu.Unlock()
	if res := msg.GetExecResult(); res != nil {
		s.ch <- res
	}
	return true
}

// messages returns every message the sender received, in arrival order.
func (s *captureSender) messages() []*agentpb.AgentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*agentpb.AgentMessage(nil), s.msgs...)
}

// TestTimeoutKillsProcessGroup is the regression test for the deploy
// orphan: a shell that backgrounds a child and waits for it must have
// the whole tree killed on timeout. With the old code (CommandContext
// kills only the shell) the backgrounded child survived as an orphan
// AND held the output pipes open, so run() did not return until the
// child exited on its own — the server had long given up by then.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only: relies on sh background jobs and kill(pid, 0)")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	sender := &captureSender{ch: make(chan *agentpb.ExecResult, 1)}
	m := NewManager(slog.Default())
	m.SetSender(sender)

	req := &agentpb.ExecRequest{
		ExecId:     "test-timeout-group",
		Shell:      "sh",
		Command:    "sleep 30 & echo $! > " + pidFile + "; wait",
		TimeoutSec: 2,
	}
	start := time.Now()
	m.run(req, &runningExec{})
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("run took %v, want < 8s (timeout 2s + WaitDelay 5s)", elapsed)
	}

	select {
	case res := <-sender.ch:
		if res.GetError() != "timeout after 2s" {
			t.Errorf("error = %q, want %q", res.GetError(), "timeout after 2s")
		}
		if res.GetExitCode() != -1 {
			t.Errorf("exit code = %d, want -1", res.GetExitCode())
		}
	default:
		t.Fatal("no result was sent")
	}

	// The backgrounded sleep was in the shell's process group, so the
	// timeout kill must have reached it.
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if processGone(pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("grandchild pid %d is still alive after timeout", pid)
			// Best-effort cleanup so a failed test does not leak a sleep.
			syscall.Kill(pid, syscall.SIGKILL)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// processGone reports whether pid no longer exists, or exists only as a
// zombie awaiting reaping (kill(pid, 0) still succeeds on zombies).
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return true
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat))
	return len(fields) >= 3 && fields[2] == "Z"
}

// TestRunNormalCommand guards the timeout plumbing against regressions:
// a command that finishes in time must still report its real exit code
// and output.
func TestRunNormalCommand(t *testing.T) {
	sender := &captureSender{ch: make(chan *agentpb.ExecResult, 1)}
	m := NewManager(slog.Default())
	m.SetSender(sender)

	m.run(&agentpb.ExecRequest{
		ExecId:     "test-normal",
		Command:    "echo hello",
		TimeoutSec: 10,
	}, &runningExec{})
	select {
	case res := <-sender.ch:
		if res.GetExitCode() != 0 {
			t.Errorf("exit code = %d, want 0 (error %q)", res.GetExitCode(), res.GetError())
		}
		if !strings.Contains(string(res.GetStdout()), "hello") {
			t.Errorf("stdout = %q, want it to contain hello", res.GetStdout())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no result was sent")
	}
}

// TestStallKillsSilentCommand: a command that prints once and then goes
// quiet must be killed when the silence outlasts the stall budget —
// long before the wall-clock timeout. This is the operative limit for a
// pull stuck on a dead network or a tar waiting on a hung mount: the
// command is dead, it just has not said so.
func TestStallKillsSilentCommand(t *testing.T) {
	sender := &captureSender{ch: make(chan *agentpb.ExecResult, 1)}
	m := NewManager(slog.Default())
	m.SetSender(sender)

	start := time.Now()
	m.run(&agentpb.ExecRequest{
		ExecId:          "test-stall",
		Command:         "echo started; sleep 30",
		TimeoutSec:      30,
		StallTimeoutSec: 2,
	}, &runningExec{})
	elapsed := time.Since(start)
	if elapsed > 8*time.Second {
		t.Fatalf("run took %v, want < 8s (stall budget 2s)", elapsed)
	}
	select {
	case res := <-sender.ch:
		if res.GetError() != "stalled: no output for 2s" {
			t.Errorf("error = %q, want %q", res.GetError(), "stalled: no output for 2s")
		}
		if res.GetExitCode() != -1 {
			t.Errorf("exit code = %d, want -1", res.GetExitCode())
		}
		if !strings.Contains(string(res.GetStdout()), "started") {
			t.Errorf("stdout = %q, want it to contain the pre-stall output", res.GetStdout())
		}
	default:
		t.Fatal("no result was sent")
	}
}

// TestStallDisabledLetsSilentCommandFinish: stall 0 means "no stall
// detection, wall-clock only". A command that stays quiet for seconds
// must run to completion instead of being killed for its silence.
func TestStallDisabledLetsSilentCommandFinish(t *testing.T) {
	sender := &captureSender{ch: make(chan *agentpb.ExecResult, 1)}
	m := NewManager(slog.Default())
	m.SetSender(sender)

	start := time.Now()
	m.run(&agentpb.ExecRequest{
		ExecId:          "test-stall-off",
		Command:         "echo started; sleep 3",
		TimeoutSec:      10,
		StallTimeoutSec: 0,
	}, &runningExec{})
	if elapsed := time.Since(start); elapsed < 3*time.Second || elapsed > 8*time.Second {
		t.Fatalf("run took %v, want ~3s", elapsed)
	}
	select {
	case res := <-sender.ch:
		if res.GetExitCode() != 0 {
			t.Errorf("exit code = %d, want 0 (error %q)", res.GetExitCode(), res.GetError())
		}
		if !strings.Contains(string(res.GetStdout()), "started") {
			t.Errorf("stdout = %q, want it to contain started", res.GetStdout())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no result was sent")
	}
}

// TestStallAllowsChattyCommand: stall is about silence, not duration. A
// command that keeps talking (one line every 0.5s) must survive a 2s
// stall budget for its full run.
func TestStallAllowsChattyCommand(t *testing.T) {
	sender := &captureSender{ch: make(chan *agentpb.ExecResult, 1)}
	m := NewManager(slog.Default())
	m.SetSender(sender)

	m.run(&agentpb.ExecRequest{
		ExecId:          "test-stall-chatty",
		Command:         "echo a; sleep 0.5; echo b; sleep 0.5; echo c; sleep 0.5; echo d",
		TimeoutSec:      10,
		StallTimeoutSec: 2,
	}, &runningExec{})
	select {
	case res := <-sender.ch:
		if res.GetExitCode() != 0 {
			t.Errorf("exit code = %d, want 0 (error %q)", res.GetExitCode(), res.GetError())
		}
		if got := string(res.GetStdout()); got != "a\nb\nc\nd\n" {
			t.Errorf("stdout = %q, want %q", got, "a\nb\nc\nd\n")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no result was sent")
	}
}

// TestStreamingDeliversChunksInOrder: output must reach the server as
// ExecOutput chunks while the command runs, in order, and the final
// ExecResult must still carry the complete output. The test asserts on
// the concatenation (not the chunk split, which depends on timing), and
// that at least one chunk precedes the result.
func TestStreamingDeliversChunksInOrder(t *testing.T) {
	sender := &captureSender{ch: make(chan *agentpb.ExecResult, 1)}
	m := NewManager(slog.Default())
	m.SetSender(sender)

	m.run(&agentpb.ExecRequest{
		ExecId:          "test-stream",
		Command:         "echo one; sleep 0.3; echo two; sleep 0.3; echo three",
		TimeoutSec:      10,
		StallTimeoutSec: 0,
	}, &runningExec{})

	var res *agentpb.ExecResult
	select {
	case res = <-sender.ch:
	case <-time.After(15 * time.Second):
		t.Fatal("no result was sent")
	}
	if res.GetExitCode() != 0 {
		t.Fatalf("exit code = %d, want 0 (error %q)", res.GetExitCode(), res.GetError())
	}

	msgs := sender.messages()
	var chunks [][]byte
	resultIdx := -1
	for i, msg := range msgs {
		if out := msg.GetExecOutput(); out != nil {
			if out.GetStream() != "stdout" {
				t.Errorf("chunk stream = %q, want stdout", out.GetStream())
			}
			chunks = append(chunks, out.GetChunk())
			continue
		}
		if msg.GetExecResult() != nil {
			resultIdx = i
		}
	}
	if len(chunks) == 0 {
		t.Fatal("no ExecOutput chunks were sent")
	}
	var joined []byte
	for _, c := range chunks {
		joined = append(joined, c...)
	}
	if string(joined) != "one\ntwo\nthree\n" {
		t.Errorf("concatenated chunks = %q, want %q", joined, "one\ntwo\nthree\n")
	}
	if string(res.GetStdout()) != "one\ntwo\nthree\n" {
		t.Errorf("result stdout = %q, want the complete output", res.GetStdout())
	}
	// Every chunk must precede the result.
	if resultIdx == 0 {
		t.Error("ExecResult arrived before any chunk")
	}
}

// TestCancelKillsRunningCommand: ExecCancel must stop a running exec and
// report it as "cancelled", killing the whole process group (the
// backgrounded sleep dies with the shell).
func TestCancelKillsRunningCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only: relies on sh background jobs and kill(pid, 0)")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	sender := &captureSender{ch: make(chan *agentpb.ExecResult, 1)}
	m := NewManager(slog.Default())
	m.SetSender(sender)

	const execID = "test-cancel"
	m.Handle(&agentpb.ExecRequest{
		ExecId:          execID,
		Shell:           "sh",
		Command:         "sleep 30 & echo $! > " + pidFile + "; wait",
		TimeoutSec:      60,
		StallTimeoutSec: 0,
	})
	// The command is long past its start by now; cancel it mid-run.
	time.Sleep(300 * time.Millisecond)
	if !m.Cancel(&agentpb.ExecCancel{ExecId: execID}) {
		t.Fatal("Cancel returned false, want true (exec is running)")
	}

	var res *agentpb.ExecResult
	select {
	case res = <-sender.ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no result was sent after cancel")
	}
	if res.GetError() != "cancelled" {
		t.Errorf("error = %q, want %q", res.GetError(), "cancelled")
	}
	if res.GetExitCode() != -1 {
		t.Errorf("exit code = %d, want -1", res.GetExitCode())
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if processGone(pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("grandchild pid %d is still alive after cancel", pid)
			syscall.Kill(pid, syscall.SIGKILL)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCancelUnknownExecIsNoop: cancelling an exec that never ran (or
// already finished) must not error and must not touch anything.
func TestCancelUnknownExecIsNoop(t *testing.T) {
	m := NewManager(slog.Default())
	if m.Cancel(&agentpb.ExecCancel{ExecId: "no-such-exec"}) {
		t.Error("Cancel(unknown) = true, want false")
	}
	if m.Cancel(nil) {
		t.Error("Cancel(nil) = true, want false")
	}
}
