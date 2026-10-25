package rpc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"watchman/proto/agentpb"
	"watchman/server/internal/audit"
	"watchman/server/internal/auth"
)

// fakeOpStream is an in-memory BidiStreamingServer for operator tests.
// Only Context/Recv/Send are exercised by the service; the embedded nil
// ServerStream would panic on header/trailer calls, which never happen here.
type fakeOpStream struct {
	grpc.ServerStream
	ctx    context.Context
	recvCh chan *agentpb.OperatorMessage
	sentCh chan *agentpb.OperatorMessage
}

func newFakeOpStream(ctx context.Context) *fakeOpStream {
	return &fakeOpStream{
		ctx:    ctx,
		recvCh: make(chan *agentpb.OperatorMessage, 16),
		sentCh: make(chan *agentpb.OperatorMessage, 16),
	}
}

func (f *fakeOpStream) Context() context.Context { return f.ctx }

func (f *fakeOpStream) Recv() (*agentpb.OperatorMessage, error) {
	m, ok := <-f.recvCh
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

func (f *fakeOpStream) Send(m *agentpb.OperatorMessage) error {
	select {
	case f.sentCh <- m:
		return nil
	case <-time.After(2 * time.Second):
		return fmt.Errorf("test: send blocked")
	}
}

func (f *fakeOpStream) closeRecv() { close(f.recvCh) }

func (f *fakeOpStream) mustRecv(t *testing.T) *agentpb.OperatorMessage {
	t.Helper()
	select {
	case m := <-f.sentCh:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no outbound frame within 2s")
		return nil
	}
}

func opCtxWithClaims(username string, role auth.Role) context.Context {
	return context.WithValue(context.Background(), operatorClaimsKey{}, &auth.Claims{Username: username, Role: role})
}

// opTestRig assembles an OperatorService against a mock-connected agent.
type opTestRig struct {
	svc     *OperatorService
	coord   *Coordinator
	hub     *Hub
	reg     *Registry
	audits  *audit.Store
	groups  map[string]bool // groups a non-admin may access
	apps    map[string]AppTargetInfo
	meshIPs map[string]string
}

func newOpTestRig(t *testing.T) *opTestRig {
	t.Helper()
	rig := &opTestRig{
		groups:  map[string]bool{"prod": true},
		apps:    map[string]AppTargetInfo{},
		meshIPs: map[string]string{},
	}
	reg, hub, coord := newLegTestRig("agent-1")
	reg.mu.Lock()
	reg.agents["agent-1"] = &Agent{ID: "agent-1", Hostname: "prod-1", Group: "prod"}
	reg.hubs["agent-1"] = hub
	reg.mu.Unlock()
	rig.reg, rig.hub, rig.coord = reg, hub, coord

	auditStore, err := audit.NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("audit store: %v", err)
	}
	rig.audits = auditStore

	rig.svc = NewOperatorService(OperatorServiceDeps{
		Registry:    reg,
		Coordinator: coord,
		Audit:       auditStore,
		Log:         slog.Default(),
		CanAccessGroup: func(username string, isAdmin bool, group string) bool {
			if isAdmin {
				return true
			}
			return rig.groups[group]
		},
		ResolveApp: func(appID string) (AppTargetInfo, bool) {
			app, ok := rig.apps[appID]
			return app, ok
		},
		MeshIP: func(hostID string) string { return rig.meshIPs[hostID] },
	})
	return rig
}

// runConnect drives svc.Connect in the background and returns its outcome channel.
func (rig *opTestRig) runConnect(stream *fakeOpStream) chan error {
	done := make(chan error, 1)
	go func() { done <- rig.svc.Connect(stream) }()
	return done
}

func (rig *opTestRig) openHostStream(t *testing.T, username string, role auth.Role, open *agentpb.OperatorOpen) (*fakeOpStream, chan error, string) {
	t.Helper()
	stream := newFakeOpStream(opCtxWithClaims(username, role))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: open}}
	ack := stream.mustRecv(t).GetAck()
	if ack == nil || !ack.GetOk() {
		t.Fatalf("expected ok ack, got %+v", ack)
	}
	return stream, done, ack.GetConnId()
}

func mustDrainTunnelOpen(t *testing.T, hub *Hub) *agentpb.TunnelOpen {
	t.Helper()
	msg, ok := hub.RecvForTest(time.Second)
	if !ok || msg.GetTunnelOpen() == nil {
		t.Fatalf("expected TunnelOpen on agent stream, got %+v", msg)
	}
	return msg.GetTunnelOpen()
}

func lastAudit(t *testing.T, st *audit.Store, action string) audit.Entry {
	t.Helper()
	entries, _ := st.List(audit.Query{Action: action})
	if len(entries) == 0 {
		t.Fatalf("no %s audit entry recorded", action)
	}
	return entries[0] // List returns newest first
}

// ---- Interceptor ----------------------------------------------------------

func opTestAuthStore(t *testing.T) *auth.Store {
	t.Helper()
	st, err := auth.NewStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	return st
}

func runInterceptor(t *testing.T, st *auth.Store, md metadata.MD) (context.Context, error) {
	t.Helper()
	stream := &fakeOpStream{
		ctx:    metadata.NewIncomingContext(context.Background(), md),
		recvCh: make(chan *agentpb.OperatorMessage),
		sentCh: make(chan *agentpb.OperatorMessage),
	}
	var gotCtx context.Context
	err := OperatorAuthInterceptor(st)(nil, stream, &grpc.StreamServerInfo{FullMethod: "/watchman.agent.OperatorService/Connect"},
		func(srv any, ss grpc.ServerStream) error {
			gotCtx = ss.Context()
			return nil
		})
	return gotCtx, err
}

func TestOperatorInterceptorRequiresToken(t *testing.T) {
	st := opTestAuthStore(t)
	if _, err := runInterceptor(t, st, metadata.MD{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated without metadata, got %v", err)
	}
	if _, err := runInterceptor(t, st, metadata.Pairs("authorization", "Bearer garbage")); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for bad token, got %v", err)
	}
}

func TestOperatorInterceptorInjectsClaims(t *testing.T) {
	st := opTestAuthStore(t)
	token, _, err := st.Authenticate("admin", "admin")
	if err != nil {
		t.Fatalf("authenticate bootstrap admin: %v", err)
	}
	ctx, err := runInterceptor(t, st, metadata.Pairs("authorization", "Bearer "+token))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	claims, ok := operatorClaimsFrom(ctx)
	if !ok || claims.Username != "admin" || claims.Role != auth.RoleAdmin {
		t.Fatalf("claims not injected correctly: %+v", claims)
	}
}

func TestOperatorInterceptorPassesThroughOtherServices(t *testing.T) {
	st := opTestAuthStore(t)
	stream := &fakeOpStream{ctx: context.Background()}
	err := OperatorAuthInterceptor(st)(nil, stream, &grpc.StreamServerInfo{FullMethod: "/watchman.agent.AgentService/Connect"},
		func(srv any, ss grpc.ServerStream) error { return nil })
	if err != nil {
		t.Fatalf("unrelated service must pass through: %v", err)
	}
}

// ---- Role / protocol gates --------------------------------------------------

func TestOperatorViewerDenied(t *testing.T) {
	rig := newOpTestRig(t)
	stream := newFakeOpStream(opCtxWithClaims("vera", auth.RoleViewer))
	err := rig.svc.Connect(stream)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
	e := lastAudit(t, rig.audits, "op_tunnel_deny")
	if e.Username != "vera" || e.Result != audit.ResultBlocked {
		t.Fatalf("deny audit mismatch: %+v", e)
	}
}

func TestOperatorFirstFrameMustBeOpen(t *testing.T) {
	rig := newOpTestRig(t)
	stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleOperator))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Data{Data: &agentpb.OperatorData{Data: []byte("x")}}}
	if err := <-done; status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

// ---- Host mode --------------------------------------------------------------

func TestOperatorHostModeLoopback(t *testing.T) {
	rig := newOpTestRig(t)
	stream, done, connID := rig.openHostStream(t, "alice", auth.RoleOperator, &agentpb.OperatorOpen{
		HostId:     "agent-1",
		TargetPort: 22,
	})
	open := mustDrainTunnelOpen(t, rig.hub)
	if open.GetTunnelId() != connID || open.GetTargetHost() != "127.0.0.1" || open.GetTargetPort() != 22 {
		t.Fatalf("unexpected tunnel open: %+v", open)
	}
	e := lastAudit(t, rig.audits, "op_tunnel_open")
	if e.RiskLevel != audit.RiskMedium || e.Result != audit.ResultSuccess || e.TargetType != "host" || e.TargetID != "agent-1" {
		t.Fatalf("open audit mismatch: %+v", e)
	}

	// Client closes cleanly: Connect returns nil, close is audited.
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Close{Close: &agentpb.OperatorClose{Reason: "done"}}}
	if err := <-done; err != nil {
		t.Fatalf("clean close should return nil, got %v", err)
	}
	lastAudit(t, rig.audits, "op_tunnel_close")

	rig.coord.mu.Lock()
	_, alive := rig.coord.legs[connID]
	rig.coord.mu.Unlock()
	if alive {
		t.Fatal("leg must be dropped after close")
	}
}

func TestOperatorCrossGroupDenied(t *testing.T) {
	rig := newOpTestRig(t)
	rig.groups = map[string]bool{} // alice sees no groups
	stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleOperator))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: &agentpb.OperatorOpen{
		HostId: "agent-1", TargetPort: 22,
	}}}
	if err := <-done; status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
	lastAudit(t, rig.audits, "op_tunnel_deny")
}

func TestOperatorUnknownHostDenied(t *testing.T) {
	rig := newOpTestRig(t)
	stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleAdmin))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: &agentpb.OperatorOpen{
		HostId: "ghost", TargetPort: 22,
	}}}
	if err := <-done; err == nil {
		t.Fatal("opening against an unknown host must fail")
	}
	lastAudit(t, rig.audits, "op_tunnel_deny")
}

func TestOperatorHostOfflineDenied(t *testing.T) {
	rig := newOpTestRig(t)
	rig.reg.mu.Lock()
	delete(rig.reg.hubs, "agent-1") // record stays from registration, stream is gone
	rig.reg.mu.Unlock()

	stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleOperator))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: &agentpb.OperatorOpen{
		HostId: "agent-1", TargetPort: 22,
	}}}
	if err := <-done; err == nil {
		t.Fatal("opening against an offline host must fail")
	}
	lastAudit(t, rig.audits, "op_tunnel_deny")
}

func TestOperatorArbitraryTargetRequiresAdmin(t *testing.T) {
	rig := newOpTestRig(t)

	// Operator role: denied, audited high.
	stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleOperator))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: &agentpb.OperatorOpen{
		HostId: "agent-1", TargetHost: "10.0.0.5", TargetPort: 3306,
	}}}
	if err := <-done; status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
	if e := lastAudit(t, rig.audits, "op_tunnel_deny"); e.RiskLevel != audit.RiskHigh {
		t.Fatalf("arbitrary-target attempt must audit high, got %s", e.RiskLevel)
	}

	// Admin: allowed, open audited high.
	stream, done, connID := rig.openHostStream(t, "root", auth.RoleAdmin, &agentpb.OperatorOpen{
		HostId: "agent-1", TargetHost: "10.0.0.5", TargetPort: 3306,
	})
	if open := mustDrainTunnelOpen(t, rig.hub); open.GetTargetHost() != "10.0.0.5" {
		t.Fatalf("target host not passed through: %+v", open)
	}
	if e := lastAudit(t, rig.audits, "op_tunnel_open"); e.RiskLevel != audit.RiskHigh {
		t.Fatalf("admin arbitrary-target open must audit high, got %s", e.RiskLevel)
	}
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Close{Close: &agentpb.OperatorClose{}}}
	<-done
	rig.coord.dropLeg(connID)
}

// ---- App mode ---------------------------------------------------------------

func (rig *opTestRig) addApp() {
	rig.apps["app-1"] = AppTargetInfo{
		HostID: "agent-1",
		Name:   "blog",
		Ports: []AppPortInfo{
			{Port: 8080, Mesh: false},
			{Port: 9090, Mesh: true},
		},
	}
}

func TestOperatorAppModePortWhitelist(t *testing.T) {
	rig := newOpTestRig(t)
	rig.addApp()

	stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleOperator))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: &agentpb.OperatorOpen{
		AppId: "app-1", TargetPort: 9999,
	}}}
	if err := <-done; status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for non-whitelisted port, got %v", err)
	}
	e := lastAudit(t, rig.audits, "op_tunnel_deny")
	if e.TargetType != "app" || e.TargetID != "app-1" {
		t.Fatalf("deny audit must reference the app: %+v", e)
	}
}

func TestOperatorAppModeResolvesPublicPort(t *testing.T) {
	rig := newOpTestRig(t)
	rig.addApp()
	_, done, connID := rig.openHostStream(t, "alice", auth.RoleOperator, &agentpb.OperatorOpen{
		AppId: "app-1", TargetPort: 8080,
	})
	open := mustDrainTunnelOpen(t, rig.hub)
	if open.GetTunnelId() != connID || open.GetTargetHost() != "127.0.0.1" || open.GetTargetPort() != 8080 {
		t.Fatalf("unexpected app tunnel open: %+v", open)
	}
	if e := lastAudit(t, rig.audits, "op_tunnel_open"); e.TargetType != "app" || e.TargetID != "app-1" {
		t.Fatalf("open audit must reference the app: %+v", e)
	}
	rig.coord.dropLeg(connID)
	<-done
}

func TestOperatorAppModeMeshBoundPort(t *testing.T) {
	rig := newOpTestRig(t)
	rig.addApp()

	// No mesh IP -> deny with a clear reason.
	stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleOperator))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: &agentpb.OperatorOpen{
		AppId: "app-1", TargetPort: 9090,
	}}}
	if err := <-done; status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected deny without mesh IP, got %v", err)
	}
	lastAudit(t, rig.audits, "op_tunnel_deny")

	// With a mesh IP the target resolves to the Tailscale address.
	rig.meshIPs["agent-1"] = "100.64.0.9"
	_, done, connID := rig.openHostStream(t, "alice", auth.RoleOperator, &agentpb.OperatorOpen{
		AppId: "app-1", TargetPort: 9090,
	})
	if open := mustDrainTunnelOpen(t, rig.hub); open.GetTargetHost() != "100.64.0.9" {
		t.Fatalf("mesh-bound port must target the mesh IP: %+v", open)
	}
	rig.coord.dropLeg(connID)
	<-done
}

// ---- Limits, pumps, teardown --------------------------------------------------

func TestOperatorPerUserCap(t *testing.T) {
	rig := newOpTestRig(t)
	for i := 0; i < maxLegsPerOperator; i++ {
		if _, _, err := rig.coord.OpenLeg(rig.hub, "127.0.0.1", 6379, "alice", "x", 0); err != nil {
			t.Fatalf("seed leg %d: %v", i, err)
		}
	}
	for i := 0; i < maxLegsPerOperator; i++ {
		_, _ = rig.hub.RecvForTest(time.Second) // drain TunnelOpens
	}

	stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleOperator))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: &agentpb.OperatorOpen{
		HostId: "agent-1", TargetPort: 22,
	}}}
	if err := <-done; status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected cap denial, got %v", err)
	}
	lastAudit(t, rig.audits, "op_tunnel_deny")

	// A different user is unaffected.
	_, done2, connID := rig.openHostStream(t, "bob", auth.RoleOperator, &agentpb.OperatorOpen{
		HostId: "agent-1", TargetPort: 22,
	})
	mustDrainTunnelOpen(t, rig.hub)
	rig.coord.dropLeg(connID)
	<-done2
}

func TestOperatorDataPumpRoundtrip(t *testing.T) {
	rig := newOpTestRig(t)
	stream, done, connID := rig.openHostStream(t, "alice", auth.RoleOperator, &agentpb.OperatorOpen{
		HostId: "agent-1", TargetPort: 6379,
	})
	mustDrainTunnelOpen(t, rig.hub)

	// client -> agent: OperatorData becomes TunnelData on the agent stream.
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Data{
		Data: &agentpb.OperatorData{Data: []byte("PING")},
	}}
	msg, ok := rig.hub.RecvForTest(time.Second)
	if !ok || msg.GetTunnelData() == nil {
		t.Fatalf("expected TunnelData, got %+v", msg)
	}
	if td := msg.GetTunnelData(); td.GetTunnelId() != connID || string(td.GetData()) != "PING" {
		t.Fatalf("unexpected TunnelData: %+v", td)
	}

	// agent -> client: handleData lands on the operator stream.
	rig.coord.handleData(&agentpb.TunnelData{TunnelId: connID, Data: []byte("PONG")})
	if got := stream.mustRecv(t).GetData(); got == nil || string(got.GetData()) != "PONG" {
		t.Fatalf("expected PONG data frame, got %+v", got)
	}

	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Close{Close: &agentpb.OperatorClose{Reason: "bye"}}}
	if err := <-done; err != nil {
		t.Fatalf("clean close should return nil, got %v", err)
	}
}

func TestOperatorAgentCloseEndsStream(t *testing.T) {
	rig := newOpTestRig(t)
	stream, done, connID := rig.openHostStream(t, "alice", auth.RoleOperator, &agentpb.OperatorOpen{
		HostId: "agent-1", TargetPort: 6379,
	})
	mustDrainTunnelOpen(t, rig.hub)

	// Agent tears down its side: the drop closes done, the pump must push an
	// OperatorClose at the client and Connect must return.
	rig.coord.handleClose(&agentpb.TunnelClose{TunnelId: connID})
	closed := stream.mustRecv(t).GetClose()
	if closed == nil {
		t.Fatal("client must receive OperatorClose when the agent side dies")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent-side teardown should return nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not return after agent-side teardown")
	}
}

func TestOperatorClientHangupEndsStream(t *testing.T) {
	rig := newOpTestRig(t)
	stream, done, connID := rig.openHostStream(t, "alice", auth.RoleOperator, &agentpb.OperatorOpen{
		HostId: "agent-1", TargetPort: 6379,
	})
	mustDrainTunnelOpen(t, rig.hub)

	stream.closeRecv() // simulates the CLI process dying / TCP reset
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("client hangup should return nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not return after client hangup")
	}

	rig.coord.mu.Lock()
	_, alive := rig.coord.legs[connID]
	rig.coord.mu.Unlock()
	if alive {
		t.Fatal("leg must be dropped after client hangup")
	}
	// The agent is told the leg is gone.
	msg, ok := rig.hub.RecvForTest(time.Second)
	if !ok || msg.GetTunnelClose() == nil || msg.GetTunnelClose().GetTunnelId() != connID {
		t.Fatalf("agent must be notified with TunnelClose, got %+v", msg)
	}
}

// ---- Infrastructure failures vs policy denials -------------------------------

// TestOperatorInfraFailureIsUnavailable guards C2: a broken leg open (agent
// stream vanished between the online check and the send) is a transient
// server-side failure — Unavailable + a failed op_tunnel_open audit, never
// an op_tunnel_deny/ResultBlocked.
func TestOperatorInfraFailureIsUnavailable(t *testing.T) {
	rig := newOpTestRig(t)
	// agent-2 exists in the registry but its hub stream is not connected
	// (no MockConnectForTest): resolveTarget's online check passes (hub is
	// in the map) while OpenLeg's hub.Send fails.
	hub2 := newHub("agent-2", 30, rig.reg)
	rig.reg.mu.Lock()
	rig.reg.agents["agent-2"] = &Agent{ID: "agent-2", Hostname: "prod-2", Group: "prod"}
	rig.reg.hubs["agent-2"] = hub2
	rig.reg.mu.Unlock()

	stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleOperator))
	done := rig.runConnect(stream)
	stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: &agentpb.OperatorOpen{
		HostId: "agent-2", TargetPort: 22,
	}}}
	if err := <-done; status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable for infra failure, got %v", err)
	}
	e := lastAudit(t, rig.audits, "op_tunnel_open")
	if e.Result != audit.ResultFailed || e.TargetID != "agent-2" {
		t.Fatalf("infra failure must audit a failed open: %+v", e)
	}
	if entries, _ := rig.audits.List(audit.Query{Action: "op_tunnel_deny"}); len(entries) != 0 {
		t.Fatalf("infra failure must not record op_tunnel_deny: %+v", entries)
	}
}

// TestOperatorPortBoundsEnforced guards C4: target_port must be 1~65535 in
// both host and app mode, denied before any leg work.
func TestOperatorPortBoundsEnforced(t *testing.T) {
	rig := newOpTestRig(t)
	rig.addApp()
	denials := []struct {
		name string
		open *agentpb.OperatorOpen
	}{
		{"host port 0", &agentpb.OperatorOpen{HostId: "agent-1"}},
		{"host port overflow", &agentpb.OperatorOpen{HostId: "agent-1", TargetPort: 70000}},
		{"app port overflow", &agentpb.OperatorOpen{AppId: "app-1", TargetPort: 70000}},
	}
	for _, d := range denials {
		t.Run(d.name, func(t *testing.T) {
			stream := newFakeOpStream(opCtxWithClaims("alice", auth.RoleOperator))
			done := rig.runConnect(stream)
			stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: d.open}}
			if err := <-done; status.Code(err) != codes.PermissionDenied {
				t.Fatalf("expected PermissionDenied, got %v", err)
			}
			if e := lastAudit(t, rig.audits, "op_tunnel_deny"); !strings.Contains(e.Detail, "目标端口") {
				t.Fatalf("deny must mention the port bound: %+v", e)
			}
		})
	}
}

// TestOperatorDenyMessagesEqualized guards C5: unknown app, unknown host and
// cross-group denial must be textually identical so an enumerator cannot
// distinguish "exists elsewhere" from "does not exist".
func TestOperatorDenyMessagesEqualized(t *testing.T) {
	rig := newOpTestRig(t)
	denyDetail := func(open *agentpb.OperatorOpen, username string, role auth.Role) string {
		stream := newFakeOpStream(opCtxWithClaims(username, role))
		done := rig.runConnect(stream)
		stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: open}}
		<-done
		return lastAudit(t, rig.audits, "op_tunnel_deny").Detail
	}

	unknownApp := denyDetail(&agentpb.OperatorOpen{AppId: "ghost-app", TargetPort: 80}, "root", auth.RoleAdmin)
	unknownHost := denyDetail(&agentpb.OperatorOpen{HostId: "ghost", TargetPort: 22}, "root", auth.RoleAdmin)
	rig.groups = map[string]bool{} // alice sees nothing
	crossGroupHost := denyDetail(&agentpb.OperatorOpen{HostId: "agent-1", TargetPort: 22}, "alice", auth.RoleOperator)

	if unknownApp != unknownHost || unknownHost != crossGroupHost {
		t.Fatalf("denial texts must be identical, got %q / %q / %q", unknownApp, unknownHost, crossGroupHost)
	}
	if !strings.Contains(unknownApp, "目标不存在或无权访问") {
		t.Fatalf("unexpected equalized denial text: %q", unknownApp)
	}
}

// Cross-group callers must not be able to probe an app's existence or its
// exposed ports: an illegal port on a cross-group app must be denied with the
// same text as an unknown app, not with the port-whitelist message. This
// locks the ordering inside resolveTarget (group check before port checks).
func TestOperatorDenyCrossGroupAppIllegalPort(t *testing.T) {
	rig := newOpTestRig(t)
	rig.apps["app-1"] = AppTargetInfo{
		HostID: "agent-1",
		Name:   "web",
		Ports:  []AppPortInfo{{Port: 8080}},
	}
	denyDetail := func(open *agentpb.OperatorOpen, username string, role auth.Role) string {
		stream := newFakeOpStream(opCtxWithClaims(username, role))
		done := rig.runConnect(stream)
		stream.recvCh <- &agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: open}}
		<-done
		return lastAudit(t, rig.audits, "op_tunnel_deny").Detail
	}

	unknownApp := denyDetail(&agentpb.OperatorOpen{AppId: "ghost-app", TargetPort: 80}, "root", auth.RoleAdmin)
	rig.groups = map[string]bool{} // alice sees nothing
	crossGroupAppBadPort := denyDetail(&agentpb.OperatorOpen{AppId: "app-1", TargetPort: 9999}, "alice", auth.RoleOperator)
	crossGroupAppGoodPort := denyDetail(&agentpb.OperatorOpen{AppId: "app-1", TargetPort: 8080}, "alice", auth.RoleOperator)
	crossGroupHostArbitrary := denyDetail(&agentpb.OperatorOpen{HostId: "agent-1", TargetHost: "10.1.2.3", TargetPort: 22}, "alice", auth.RoleOperator)

	for name, got := range map[string]string{
		"cross-group app illegal port": crossGroupAppBadPort,
		"cross-group app legal port":   crossGroupAppGoodPort,
		"cross-group host arbitrary":   crossGroupHostArbitrary,
	} {
		if got != unknownApp {
			t.Fatalf("%s denial = %q, want identical to unknown app %q", name, got, unknownApp)
		}
		if strings.Contains(got, "白名单") || strings.Contains(got, "仅管理员") {
			t.Fatalf("%s denial leaks app/host details: %q", name, got)
		}
	}
}
