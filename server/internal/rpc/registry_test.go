package rpc

import (
	"log/slog"
	"testing"
	"time"

	"watchman/proto/agentpb"
)

// TestListAgentsStableOrder verifies ListAgents returns a deterministic order.
// Go map iteration is randomized, so without an explicit sort the host list
// reshuffles on every refresh — the bug this guards against.
func TestListAgentsStableOrder(t *testing.T) {
	r := NewRegistry("", slog.Default())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Insert out of order; registration time should drive the result order.
	r.agents["zzz"] = &Agent{ID: "zzz", Hostname: "c", Registered: base.Add(2 * time.Minute)}
	r.agents["aaa"] = &Agent{ID: "aaa", Hostname: "a", Registered: base}
	r.agents["mmm"] = &Agent{ID: "mmm", Hostname: "b", Registered: base.Add(1 * time.Minute)}

	var first []string
	for _, a := range r.ListAgents() {
		first = append(first, a.ID)
	}
	want := []string{"aaa", "mmm", "zzz"}
	for i, id := range want {
		if first[i] != id {
			t.Fatalf("order[%d] = %q, want %q (full: %v)", i, first[i], id, first)
		}
	}

	// Repeated calls must return the identical order (map randomization must not
	// leak through).
	for iter := 0; iter < 50; iter++ {
		got := r.ListAgents()
		for i := range want {
			if got[i].ID != want[i] {
				t.Fatalf("iteration %d order changed: %q != %q", iter, got[i].ID, want[i])
			}
		}
	}
}

// TestListAgentsTiebreakByID checks agents with an identical (or zero)
// registration timestamp fall back to a stable ID sort rather than random map
// order.
func TestListAgentsTiebreakByID(t *testing.T) {
	r := NewRegistry("", slog.Default())
	// All zero timestamps => ID is the only tiebreaker.
	for _, id := range []string{"delta", "alpha", "charlie", "bravo"} {
		r.agents[id] = &Agent{ID: id, Hostname: id}
	}
	want := []string{"alpha", "bravo", "charlie", "delta"}
	for iter := 0; iter < 30; iter++ {
		got := r.ListAgents()
		for i := range want {
			if got[i].ID != want[i] {
				t.Fatalf("iteration %d: order[%d] = %q, want %q", iter, i, got[i].ID, want[i])
			}
		}
	}
}

// TestUnbindWithReentrantHandlerNoDeadlock reproduces the production alert
// freeze: unbind used to invoke resp handlers while holding hub.mu, and the
// metrics-poll handler's first statement re-enters SetRespHandler to
// deregister itself — a self-deadlock on the non-reentrant mutex that pinned
// hub.mu forever, freezing the alert monitor (LastMetrics) and the metrics
// collector. The test runs unbind while such a handler is registered; if the
// deadlock regresses, the test times out rather than hanging forever.
func TestUnbindWithReentrantHandlerNoDeadlock(t *testing.T) {
	r := NewRegistry("", slog.Default())
	hub := newHub("agent-1", 30, r)
	hub.MockConnectForTest()

	// Exactly like metrics.Store.StartAutoCollector's one-shot poll handler.
	hub.SetRespHandler("metrics-poll", func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler("metrics-poll", nil) // re-enter while being invoked
	})

	done := make(chan struct{})
	go func() {
		hub.unbind()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("unbind deadlocked: resp handler re-entered hub.mu while held")
	}

	// The hub must remain fully usable after unbind.
	hub.mu.Lock()
	if len(hub.respHandlers) != 0 {
		t.Fatalf("respHandlers not cleared: %d entries left", len(hub.respHandlers))
	}
	if !hub.closed {
		t.Fatal("hub.closed flag not set by unbind")
	}
	hub.mu.Unlock()

	// Send after close must return false instead of panicking on the closed
	// channel (bind() may race a reconnect).
	if hub.Send(&agentpb.ServerMessage{}) {
		t.Fatal("Send on unbound hub should return false")
	}
}

// TestUnbindThenRebindChannelFreshness verifies the reconnect path: after a
// stale hub is unbound, a NEW hub replaces it in the registry (Register
// creates a fresh hub per connection), so the closed sendCh of the old hub
// never receives traffic. Simulates the sequence the flaky Singapore host
// exercises every reconnect.
func TestUnbindThenRebindChannelFreshness(t *testing.T) {
	r := NewRegistry("", slog.Default())
	hub := newHub("agent-1", 30, r)
	hub.MockConnectForTest()

	hub.SetRespHandler("metrics-poll", func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler("metrics-poll", nil)
	})
	hub.SetTermHandler("sess-1", func(out *agentpb.TerminalOutput) {})

	hub.unbind()

	// The closed channel is drained by unbind-time consumers; just assert the
	// hub reports itself closed and refuses further sends.
	hub.mu.Lock()
	closed := hub.closed
	hub.mu.Unlock()
	if !closed {
		t.Fatal("sendCh should be closed after unbind")
	}

	// A fresh hub (as created by Register on reconnect) must be unaffected.
	hub2 := newHub("agent-1", 30, r)
	hub2.MockConnectForTest()
	if !hub2.Send(&agentpb.ServerMessage{}) {
		t.Fatal("fresh hub should accept sends")
	}
	msg, ok := hub2.RecvForTest(time.Second)
	if !ok || msg == nil {
		t.Fatal("fresh hub did not deliver message")
	}
	hub2.unbind()
}

// TestSendConcurrentUnbindNoStarvationOrPanic verifies that when sendCh is full,
// a blocking Send does not cause unbind() to stall or panic when unbind() closes sendCh.
func TestSendConcurrentUnbindNoStarvationOrPanic(t *testing.T) {
	r := NewRegistry("", slog.Default())
	hub := newHub("agent-1", 30, r)
	hub.MockConnectForTest()

	// Fill sendCh to capacity so next Send will block in select
	for i := 0; i < cap(hub.sendCh); i++ {
		hub.sendCh <- &agentpb.ServerMessage{}
	}

	sendDone := make(chan bool, 1)
	go func() {
		// This Send will block because sendCh is full
		ok := hub.Send(&agentpb.ServerMessage{})
		sendDone <- ok
	}()

	// Give the goroutine time to enter select
	time.Sleep(50 * time.Millisecond)

	unbindStart := time.Now()
	unbindDone := make(chan struct{})
	go func() {
		hub.unbind()
		close(unbindDone)
	}()

	select {
	case <-unbindDone:
		if dur := time.Since(unbindStart); dur > 1*time.Second {
			t.Fatalf("unbind took too long (%v), write starvation occurred", dur)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unbind hung waiting for Send lock")
	}

	select {
	case sent := <-sendDone:
		if sent {
			t.Fatal("Send should have returned false after unbind closed channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send hung and did not recover on channel close")
	}
}

