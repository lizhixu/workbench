package rpc

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"watchman/proto/agentpb"
)

// fakeConnectServer captures the messages the registry sends to an agent.
// It only implements Send/Recv; the remaining grpc.ServerStream methods
// come from the embedded (nil) interface and are never called by the
// registry's send pump.
type fakeConnectServer struct {
	grpc.ServerStream
	sent chan *agentpb.ServerMessage
}

func (f *fakeConnectServer) Send(msg *agentpb.ServerMessage) error {
	f.sent <- msg
	return nil
}

func (f *fakeConnectServer) Recv() (*agentpb.AgentMessage, error) {
	return nil, nil
}

func (f *fakeConnectServer) Context() context.Context { return context.Background() }
func (f *fakeConnectServer) SetHeader(metadata.MD) error {
	return nil
}
func (f *fakeConnectServer) SendHeader(metadata.MD) error { return nil }
func (f *fakeConnectServer) SetTrailer(metadata.MD)       {}
func (f *fakeConnectServer) SendMsg(m any) error          { return nil }
func (f *fakeConnectServer) RecvMsg(m any) error          { return nil }

func newDialTargetRegistry() *Registry {
	r := NewRegistry("", slog.Default())
	r.DialTarget = func() string { return "35.212.206.45:9090" }
	return r
}

func TestRegisterResponseCarriesDialTarget(t *testing.T) {
	r := newDialTargetRegistry()
	r.agents["a1"] = &Agent{ID: "a1", Hostname: "old", AuthToken: "tok1"}
	r.tokens["tok1"] = "a1"

	// A new agent reports its dial address and a failed target; the
	// response carries the dial address and the record stores both.
	_, resp, err := r.Register(context.Background(), &agentpb.RegisterRequest{
		AgentId:              "a1",
		Hostname:             "h1",
		ServerAddr:           "192.255.178.173:9090",
		RedirectFailedTarget: "35.212.206.45:9090",
	}, "tok1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := resp.GetServerAddr(); got != "35.212.206.45:9090" {
		t.Fatalf("RegisterResponse.ServerAddr = %q", got)
	}
	a := r.agents["a1"]
	if a.ServerAddr != "192.255.178.173:9090" {
		t.Fatalf("Agent.ServerAddr = %q", a.ServerAddr)
	}
	if a.RedirectFailedTarget != "35.212.206.45:9090" {
		t.Fatalf("Agent.RedirectFailedTarget = %q", a.RedirectFailedTarget)
	}

	// An old agent sends neither field: the stored values must survive.
	if _, _, err := r.Register(context.Background(), &agentpb.RegisterRequest{
		AgentId:  "a1",
		Hostname: "h1",
	}, "tok1"); err != nil {
		t.Fatalf("Register (old agent): %v", err)
	}
	if a.ServerAddr != "192.255.178.173:9090" || a.RedirectFailedTarget != "35.212.206.45:9090" {
		t.Fatalf("old-agent register cleared stored values: %q / %q", a.ServerAddr, a.RedirectFailedTarget)
	}

	// A fresh enroll gets the dial address too.
	tok := r.IssueEnrollToken()
	_, resp, err = r.Register(context.Background(), &agentpb.RegisterRequest{
		EnrollToken: tok,
		AgentId:     "new1",
		Hostname:    "h2",
	}, "")
	if err != nil {
		t.Fatalf("Register (enroll): %v", err)
	}
	if got := resp.GetServerAddr(); got != "35.212.206.45:9090" {
		t.Fatalf("enroll RegisterResponse.ServerAddr = %q", got)
	}

	// No provider means no dial address.
	r2 := NewRegistry("", slog.Default())
	r2.agents["a1"] = &Agent{ID: "a1", Hostname: "old", AuthToken: "tok1"}
	r2.tokens["tok1"] = "a1"
	_, resp, err = r2.Register(context.Background(), &agentpb.RegisterRequest{
		AgentId: "a1",
	}, "tok1")
	if err != nil {
		t.Fatalf("Register (no provider): %v", err)
	}
	if got := resp.GetServerAddr(); got != "" {
		t.Fatalf("RegisterResponse.ServerAddr without provider = %q", got)
	}
}

func TestPushRuntimeConfigCarriesDialTarget(t *testing.T) {
	r := newDialTargetRegistry()
	r.agents["a1"] = &Agent{ID: "a1", Hostname: "h1", TrafficResetDay: 5}
	hub := newHub("a1", 30, r)
	fake := &fakeConnectServer{sent: make(chan *agentpb.ServerMessage, 4)}
	hub.bind(fake)
	r.hubs["a1"] = hub
	defer hub.unbind()

	if !r.PushRuntimeConfig("a1") {
		t.Fatalf("PushRuntimeConfig returned false")
	}
	select {
	case msg := <-fake.sent:
		cu := msg.GetConfigUpdate()
		if cu == nil {
			t.Fatalf("pushed message is not a ConfigUpdate")
		}
		if got := cu.GetServerAddr(); got != "35.212.206.45:9090" {
			t.Fatalf("ConfigUpdate.ServerAddr = %q", got)
		}
		if got := cu.GetTrafficResetDay(); got != 5 {
			t.Fatalf("ConfigUpdate.TrafficResetDay = %d", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no ConfigUpdate received")
	}

	// Offline agent: no push.
	if r.PushRuntimeConfig("missing") {
		t.Fatalf("PushRuntimeConfig for unknown agent returned true")
	}
}

func TestBroadcastRuntimeConfig(t *testing.T) {
	r := newDialTargetRegistry()
	r.agents["a1"] = &Agent{ID: "a1", Hostname: "h1"}
	r.agents["a2"] = &Agent{ID: "a2", Hostname: "h2"}

	// a1 has a live stream; a2 has a hub without one (Send fails).
	hub1 := newHub("a1", 30, r)
	fake := &fakeConnectServer{sent: make(chan *agentpb.ServerMessage, 4)}
	hub1.bind(fake)
	r.hubs["a1"] = hub1
	defer hub1.unbind()
	r.hubs["a2"] = newHub("a2", 30, r)

	if n := r.BroadcastRuntimeConfig(); n != 1 {
		t.Fatalf("BroadcastRuntimeConfig = %d, want 1 (only the live hub counts)", n)
	}
	select {
	case msg := <-fake.sent:
		if got := msg.GetConfigUpdate().GetServerAddr(); got != "35.212.206.45:9090" {
			t.Fatalf("broadcast ConfigUpdate.ServerAddr = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no broadcast ConfigUpdate received")
	}

	// No hubs at all: zero, no hang.
	r2 := newDialTargetRegistry()
	if n := r2.BroadcastRuntimeConfig(); n != 0 {
		t.Fatalf("BroadcastRuntimeConfig with no hubs = %d", n)
	}
}
