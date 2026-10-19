package rpc

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"watchman/proto/agentpb"
)

// newLegTestRig wires a registry, one mock-connected hub and a coordinator.
func newLegTestRig(agentID string) (*Registry, *Hub, *Coordinator) {
	reg := NewRegistry("", slog.Default())
	hub := newHub(agentID, 30, reg)
	hub.MockConnectForTest()
	coord := NewTunnelCoordinator(reg, slog.Default())
	return reg, hub, coord
}

func TestOpenLegSendsTunnelOpen(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")

	connID, inbound, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "127.0.0.1:6379", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}
	if connID == "" || inbound == nil {
		t.Fatal("expected conn id and inbound channel")
	}

	msg, ok := hub.RecvForTest(time.Second)
	if !ok {
		t.Fatal("expected TunnelOpen on agent stream")
	}
	open := msg.GetTunnelOpen()
	if open == nil {
		t.Fatalf("expected TunnelOpen, got %T", msg.Payload)
	}
	if open.GetTunnelId() != connID || open.GetTargetHost() != "127.0.0.1" || open.GetTargetPort() != 6379 {
		t.Fatalf("unexpected open frame: %+v", open)
	}

	coord.mu.Lock()
	tc := coord.legs[connID]
	coord.mu.Unlock()
	if tc == nil || tc.owner != "alice" {
		t.Fatalf("leg not registered with owner: %+v", tc)
	}
	if tc.sock != nil {
		t.Fatal("stream leg must not hold a local socket")
	}
}

func TestOpenLegRejectsBadArgs(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	if _, _, err := coord.OpenLeg(nil, "127.0.0.1", 6379, "alice", "x", 0); err == nil {
		t.Fatal("expected error for nil hub")
	}
	if _, _, err := coord.OpenLeg(hub, "", 6379, "alice", "x", 0); err == nil {
		t.Fatal("expected error for empty target host")
	}
	if _, _, err := coord.OpenLeg(hub, "127.0.0.1", 0, "alice", "x", 0); err == nil {
		t.Fatal("expected error for zero target port")
	}
}

func TestOpenLegCapsStreamLegsPerAgent(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	for i := 0; i < maxStreamLegsPerAgent; i++ {
		if _, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0); err != nil {
			t.Fatalf("leg %d: %v", i, err)
		}
	}
	_, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0)
	if !errors.Is(err, ErrAgentLegsExceeded) {
		t.Fatalf("expected ErrAgentLegsExceeded beyond maxStreamLegsPerAgent, got %v", err)
	}
}

// TestOpenLegOwnerCap guards the per-operator limit moved into OpenLeg:
// ownerLimit=0 disables it and one owner's legs never count for another.
func TestOpenLegOwnerCap(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	for i := 0; i < 3; i++ {
		if _, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 3); err != nil {
			t.Fatalf("leg %d: %v", i, err)
		}
	}
	_, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 3)
	if !errors.Is(err, ErrOwnerLegsExceeded) {
		t.Fatalf("expected ErrOwnerLegsExceeded, got %v", err)
	}
	if _, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0); err != nil {
		t.Fatalf("unlimited open must succeed: %v", err)
	}
	if _, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "bob", "x", 3); err != nil {
		t.Fatalf("bob must not count alice's legs: %v", err)
	}
}

// TestOpenLegOwnerCapAtomicUnderConcurrency is the TOCTOU regression gate:
// the cap check and leg insert hold one mutex, otherwise a fan-in of
// concurrent opens overshoots the limit.
func TestOpenLegOwnerCapAtomicUnderConcurrency(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	stop := make(chan struct{})
	defer close(stop)
	go func() { // drain TunnelOpen frames so hub.Send never blocks
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = hub.RecvForTest(50 * time.Millisecond)
			}
		}
	}()

	const workers, limit = 16, 4
	var okCnt atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "carol", "x", limit); err == nil {
				okCnt.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := okCnt.Load(); got != limit {
		t.Fatalf("exactly %d opens must succeed under the cap, got %d", limit, got)
	}
}

func TestHandleDataRoutesToStreamLeg(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	connID, inbound, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}
	_, _ = hub.RecvForTest(time.Second) // drain TunnelOpen

	coord.handleData(&agentpb.TunnelData{TunnelId: connID, Data: []byte("ping")})
	select {
	case data := <-inbound:
		if string(data) != "ping" {
			t.Fatalf("unexpected payload %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("payload did not reach inbound channel")
	}

	// Activity must refresh the idle deadline (otherwise a busy leg looks stale).
	coord.mu.Lock()
	before := coord.legs[connID].lastActive.Load()
	coord.mu.Unlock()
	coord.handleData(&agentpb.TunnelData{TunnelId: connID, Data: []byte("pong")})
	coord.mu.Lock()
	after := coord.legs[connID].lastActive.Load()
	coord.mu.Unlock()
	if after < before {
		t.Fatal("lastActive must not move backwards")
	}
}

func TestSlowConsumerStreamLegDropped(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	connID, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}
	_, _ = hub.RecvForTest(time.Second) // drain TunnelOpen

	// Nobody drains toSock: the 65th payload overflows the 64-deep buffer and
	// the leg must be torn down instead of stalling the agent recv loop.
	for i := 0; i < 64; i++ {
		coord.handleData(&agentpb.TunnelData{TunnelId: connID, Data: []byte("x")})
	}
	coord.handleData(&agentpb.TunnelData{TunnelId: connID, Data: []byte("x")})

	coord.mu.Lock()
	_, alive := coord.legs[connID]
	coord.mu.Unlock()
	if alive {
		t.Fatal("slow-consumer leg must be dropped")
	}

	// Teardown notifies the agent with TunnelClose.
	msg, ok := hub.RecvForTest(time.Second)
	if !ok || msg.GetTunnelClose() == nil || msg.GetTunnelClose().GetTunnelId() != connID {
		t.Fatalf("expected TunnelClose for dropped leg, got %+v", msg)
	}
}

func TestDropStreamLegNotifiesAgentOnce(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	connID, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}
	_, _ = hub.RecvForTest(time.Second)

	coord.dropLeg(connID)
	coord.dropLeg(connID) // idempotent

	msg, ok := hub.RecvForTest(time.Second)
	if !ok || msg.GetTunnelClose() == nil || msg.GetTunnelClose().GetTunnelId() != connID {
		t.Fatalf("expected TunnelClose, got %+v", msg)
	}
	if msg, ok = hub.RecvForTest(50 * time.Millisecond); ok {
		t.Fatalf("second drop must not re-notify, got %+v", msg)
	}
}

func TestCloseByAgentSweepsOrphanStreamLegs(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	connID, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}
	_, _ = hub.RecvForTest(time.Second)

	coord.CloseByAgent("agent-1")

	coord.mu.Lock()
	_, alive := coord.legs[connID]
	coord.mu.Unlock()
	if alive {
		t.Fatal("orphan stream leg must die with the old agent stream")
	}
}

func TestListAndCloseOwnerLegs(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	connID, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "127.0.0.1:6379", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}
	_, _ = hub.RecvForTest(time.Second)

	legs := coord.ListOwnerLegs("alice")
	if len(legs) != 1 || legs[0].ConnID != connID || legs[0].Target != "127.0.0.1:6379" || legs[0].AgentID != "agent-1" {
		t.Fatalf("unexpected listing: %+v", legs)
	}
	if len(coord.ListOwnerLegs("bob")) != 0 {
		t.Fatal("bob must not see alice's legs")
	}

	if err := coord.CloseOwnerLeg(connID, "bob", false); err == nil {
		t.Fatal("other users must not close alice's leg")
	}
	if err := coord.CloseOwnerLeg(connID, "alice", false); err != nil {
		t.Fatalf("owner close: %v", err)
	}
	if err := coord.CloseOwnerLeg(connID, "alice", false); err == nil {
		t.Fatal("closing a dead leg must error")
	}

	// Admin force-closes another user's leg.
	connID2, _, err := coord.OpenLeg(hub, "127.0.0.1", 3306, "bob", "127.0.0.1:3306", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}
	if err := coord.CloseOwnerLeg(connID2, "alice", true); err != nil {
		t.Fatalf("admin close: %v", err)
	}
}

func TestReaperReclaimsIdleAndOveragedLegs(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	idleID, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}
	freshID, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}

	coord.mu.Lock()
	coord.legs[idleID].lastActive.Store(time.Now().Add(-operatorLegIdleTimeout - time.Minute).UnixNano())
	coord.mu.Unlock()

	coord.reapStaleLegs(time.Now())

	coord.mu.Lock()
	_, idleAlive := coord.legs[idleID]
	_, freshAlive := coord.legs[freshID]
	coord.mu.Unlock()
	if idleAlive {
		t.Fatal("idle leg must be reclaimed")
	}
	if !freshAlive {
		t.Fatal("fresh leg must survive the sweep")
	}

	// A constantly-active leg still dies at the hard lifetime cap.
	coord.mu.Lock()
	coord.legs[freshID].createdAt = time.Now().Add(-operatorLegMaxLifetime - time.Minute)
	coord.legs[freshID].touch()
	coord.mu.Unlock()
	coord.reapStaleLegs(time.Now())
	coord.mu.Lock()
	_, freshAlive = coord.legs[freshID]
	coord.mu.Unlock()
	if freshAlive {
		t.Fatal("over-aged leg must be reclaimed despite activity")
	}
}

func TestTouchLeg(t *testing.T) {
	_, hub, coord := newLegTestRig("agent-1")
	connID, _, err := coord.OpenLeg(hub, "127.0.0.1", 6379, "alice", "x", 0)
	if err != nil {
		t.Fatalf("OpenLeg: %v", err)
	}
	coord.mu.Lock()
	before := coord.legs[connID].lastActive.Load()
	coord.legs[connID].lastActive.Store(before - int64(time.Hour))
	coord.mu.Unlock()

	coord.TouchLeg(connID)

	coord.mu.Lock()
	after := coord.legs[connID].lastActive.Load()
	coord.mu.Unlock()
	if after < before {
		t.Fatal("TouchLeg must refresh activity")
	}
	coord.TouchLeg("does-not-exist") // no panic
}
