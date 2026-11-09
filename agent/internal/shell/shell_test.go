package shell

import (
	"strings"
	"sync"
	"testing"
	"time"

	"watchman/proto/agentpb"
)

// fakeSender records agent->server messages sent by the manager.
type fakeSender struct {
	mu   sync.Mutex
	msgs []*agentpb.AgentMessage
}

func (f *fakeSender) Send(msg *agentpb.AgentMessage) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, msg)
	return true
}

func (f *fakeSender) outputs() []*agentpb.TerminalOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var outs []*agentpb.TerminalOutput
	for _, m := range f.msgs {
		if o, ok := m.Payload.(*agentpb.AgentMessage_TermOut); ok {
			outs = append(outs, o.TermOut)
		}
	}
	return outs
}

func TestOpenFailureSurfacesError(t *testing.T) {
	m := NewManager(nil)
	sender := &fakeSender{}
	m.SetSender(sender)

	err := m.Open(&agentpb.TerminalOpen{
		SessionId: "sid-fail",
		Shell:     "watchman-definitely-not-a-binary-xyz",
		Cols:      80,
		Rows:      24,
	})
	if err == nil {
		t.Fatal("Open with a nonexistent binary should fail")
	}
	outs := sender.outputs()
	if len(outs) != 2 {
		t.Fatalf("expected 2 terminal outputs (error + EOF), got %d", len(outs))
	}
	if outs[0].GetSessionId() != "sid-fail" {
		t.Errorf("first output session = %q, want sid-fail", outs[0].GetSessionId())
	}
	if got := string(outs[0].GetData()); !strings.Contains(got, "无法启动终端") || !strings.Contains(got, "open pty") {
		t.Errorf("first output = %q, want it to carry the open error", got)
	}
	if len(outs[1].GetData()) != 0 {
		t.Errorf("second output should be empty (EOF signal), got %q", outs[1].GetData())
	}
	// The failed session must not be registered: input to it is unknown.
	if err := m.Input(&agentpb.TerminalInput{SessionId: "sid-fail", Data: []byte("x")}); err == nil {
		t.Error("Input to a failed session should report unknown session")
	}
}

func TestOpenMultiWordShell(t *testing.T) {
	m := NewManager(nil)
	sender := &fakeSender{}
	m.SetSender(sender)

	// `echo` exits at once; Open must still succeed and the pump must deliver
	// its output before signalling EOF.
	err := m.Open(&agentpb.TerminalOpen{
		SessionId: "sid-echo",
		Shell:     "echo hello-pty",
		Cols:      80,
		Rows:      24,
	})
	if err != nil {
		t.Fatalf("Open with a multi-word shell failed: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var outs []*agentpb.TerminalOutput
	for time.Now().Before(deadline) {
		outs = sender.outputs()
		if len(outs) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(outs) < 2 {
		t.Fatalf("expected output + EOF for echo session, got %d outputs", len(outs))
	}
	if got := string(outs[0].GetData()); !strings.Contains(got, "hello-pty") {
		t.Errorf("first output = %q, want it to contain hello-pty", got)
	}
	if len(outs[1].GetData()) != 0 {
		t.Errorf("second output should be empty (EOF signal), got %q", outs[1].GetData())
	}
	_ = m.Close(&agentpb.TerminalClose{SessionId: "sid-echo"})
}

func TestOpenSingleWordShell(t *testing.T) {
	m := NewManager(nil)
	m.SetSender(&fakeSender{})

	if err := m.Open(&agentpb.TerminalOpen{
		SessionId: "sid-sh",
		Shell:     "sh",
		Cols:      80,
		Rows:      24,
	}); err != nil {
		t.Fatalf("Open with single-word shell failed: %v", err)
	}
	if err := m.Close(&agentpb.TerminalClose{SessionId: "sid-sh"}); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}
