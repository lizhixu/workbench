package apps

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

// execTestServer is a real in-process control plane (registry + gRPC
// server on a loopback listener) plus a fake agent stream. The fake
// agent's received ServerMessages land on recvCh; the test sends
// AgentMessages back through stream.Send, driving both sides of the
// exec protocol without a real host.
type execTestServer struct {
	t      *testing.T
	reg    *rpc.Registry
	hub    *rpc.Hub
	stream agentpb.AgentService_ConnectClient
	recvCh chan *agentpb.ServerMessage
}

func newExecTestServer(t *testing.T) *execTestServer {
	t.Helper()
	reg := rpc.NewRegistry(t.TempDir(), nil)
	srv := rpc.NewServer(reg, slog.Default())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	client := agentpb.NewAgentServiceClient(cc)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	tok := reg.IssueEnrollToken()
	if err := stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Register{
			Register: &agentpb.RegisterRequest{
				EnrollToken: tok, AgentId: "test-agent", Hostname: "test-agent", Os: "linux", Arch: "amd64",
			},
		},
	}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv register response: %v", err)
	}
	if !first.GetRegister().GetOk() {
		t.Fatalf("register rejected: %v", first.GetRegister().GetError())
	}
	hub := reg.Hub("test-agent")
	if hub == nil {
		t.Fatal("no hub for test-agent after register")
	}

	s := &execTestServer{t: t, reg: reg, hub: hub, stream: stream, recvCh: make(chan *agentpb.ServerMessage, 64)}
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				close(s.recvCh)
				return
			}
			s.recvCh <- msg
		}
	}()
	return s
}

// waitExecRequest returns the next ExecRequest the fake agent receives.
func (s *execTestServer) waitExecRequest(t *testing.T) *agentpb.ExecRequest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case msg, ok := <-s.recvCh:
			if !ok {
				t.Fatal("agent stream closed")
			}
			if req := msg.GetExec(); req != nil {
				return req
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("no ExecRequest arrived within 10s")
	return nil
}

type execOutcome struct {
	res *agentpb.ExecResult
	err error
}

// TestExecOnAgentStreamsOutputIntoLog: live ExecOutput chunks must reach
// the deployment log while the step is still running (not only in the
// final snapshot), and the final log must contain each byte exactly once.
func TestExecOnAgentStreamsOutputIntoLog(t *testing.T) {
	s := newExecTestServer(t)
	logBuf := &logBuffer{}
	const execID = "stream-test-1"
	outCh := make(chan execOutcome, 1)
	go func() {
		res, err := execOnAgent(context.Background(), s.hub, execID, "echo hi", 30, 120, logBuf)
		outCh <- execOutcome{res, err}
	}()
	req := s.waitExecRequest(t)
	if req.GetExecId() != execID {
		t.Fatalf("exec_id = %q, want %q", req.GetExecId(), execID)
	}
	if req.GetStallTimeoutSec() != 120 {
		t.Errorf("stall_timeout_sec = %d, want 120", req.GetStallTimeoutSec())
	}
	send := func(stream, chunk string) {
		t.Helper()
		if err := s.stream.Send(&agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_ExecOutput{
				ExecOutput: &agentpb.ExecOutput{ExecId: execID, Chunk: []byte(chunk), Stream: stream},
			},
		}); err != nil {
			t.Fatalf("send chunk: %v", err)
		}
	}
	send("stdout", "line1\n")
	// The chunk must be visible in the live log before the result lands.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logBuf.String(), "line1\n") {
		if time.Now().After(deadline) {
			t.Fatal("chunk did not reach the live log within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	send("stdout", "line2\n")
	send("stderr", "warn\n")
	if err := s.stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_ExecResult{
			ExecResult: &agentpb.ExecResult{ExecId: execID, ExitCode: 0, Stdout: []byte("line1\nline2\n"), Stderr: []byte("warn\n")},
		},
	}); err != nil {
		t.Fatalf("send result: %v", err)
	}
	select {
	case out := <-outCh:
		if out.err != nil {
			t.Fatalf("execOnAgent error: %v", out.err)
		}
		if out.res.GetExitCode() != 0 {
			t.Errorf("exit code = %d, want 0", out.res.GetExitCode())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execOnAgent did not return after the result")
	}
	if got := logBuf.String(); got != "line1\nline2\n[stderr]\nwarn\n" {
		t.Errorf("log = %q, want %q", got, "line1\nline2\n[stderr]\nwarn\n")
	}
}

// TestExecOnAgentCancelsOnContextDone: when the pipeline's context is
// cancelled while an exec is running, execOnAgent must return promptly
// with context.Canceled AND tell the agent to stop the exec (ExecCancel),
// so the command does not keep running on the host.
func TestExecOnAgentCancelsOnContextDone(t *testing.T) {
	s := newExecTestServer(t)
	logBuf := &logBuffer{}
	const execID = "cancel-test-1"
	ctx, cancel := context.WithCancel(context.Background())
	outCh := make(chan execOutcome, 1)
	go func() {
		res, err := execOnAgent(ctx, s.hub, execID, "sleep 60", 30, 120, logBuf)
		outCh <- execOutcome{res, err}
	}()
	req := s.waitExecRequest(t)
	if req.GetExecId() != execID {
		t.Fatalf("exec_id = %q, want %q", req.GetExecId(), execID)
	}
	// The agent stays silent (the command is still running); cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case out := <-outCh:
		if !errors.Is(out.err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", out.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execOnAgent did not return after ctx cancel")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case msg, ok := <-s.recvCh:
			if !ok {
				t.Fatal("agent stream closed")
			}
			if c := msg.GetExecCancel(); c != nil {
				if c.GetExecId() != execID {
					t.Errorf("cancel exec_id = %q, want %q", c.GetExecId(), execID)
				}
				return
			}
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no ExecCancel arrived within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFailAfterCancelKeepsCancelled: CancelDeployment removes the running
// entry the moment it marks the record cancelled, so a pipeline goroutine
// that is still unwinding (e.g. an execOnAgent wait that just returned
// context.Canceled) must not overwrite the record with a failure. The
// race is real: CancelDeployment deletes the entry first and marks the
// record second, so the record's own terminal guard cannot catch a
// fail() that lands in between.
func TestFailAfterCancelKeepsCancelled(t *testing.T) {
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	reg := rpc.NewRegistry(t.TempDir(), nil)
	e := NewEngine(reg, store, nil, slog.Default())
	app := putUpgradeTestApp(t, store, &Application{ID: "app1", HostID: "h1", SourceType: "image", Image: "x:latest"})
	dep := &Deployment{ID: "dep1", AppID: app.ID, Status: DeployBuilding, StartedAt: time.Now()}
	if err := store.AddDeployment(dep); err != nil {
		t.Fatalf("AddDeployment: %v", err)
	}
	_, cancel := context.WithCancel(context.Background())
	if err := e.registerRunning(app.ID, &activeDeployment{depID: dep.ID, cancel: cancel}); err != nil {
		t.Fatalf("registerRunning: %v", err)
	}
	if !e.CancelDeployment(app.ID, "tester") {
		t.Fatal("CancelDeployment returned false")
	}
	e.fail(app, dep, &logBuffer{}, "boom")
	got, ok := store.GetDeployment(app.ID, dep.ID)
	if !ok {
		t.Fatal("deployment record vanished")
	}
	if got.Status != DeployCancelled {
		t.Errorf("status = %q, want %q", got.Status, DeployCancelled)
	}
	if got.Error != "部署已被用户取消" {
		t.Errorf("error = %q, want the cancel message", got.Error)
	}
	if got.CancelledBy != "tester" {
		t.Errorf("cancelled_by = %q, want tester", got.CancelledBy)
	}
	if strings.Contains(got.BuildLog, "[FAILED]") {
		t.Errorf("build log contains a failure marker after cancel: %q", got.BuildLog)
	}
}

// TestFailMarksFailedWhileRunning is the control for the guard above:
// without a cancel, fail() still records the failure.
func TestFailMarksFailedWhileRunning(t *testing.T) {
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	reg := rpc.NewRegistry(t.TempDir(), nil)
	e := NewEngine(reg, store, nil, slog.Default())
	app := putUpgradeTestApp(t, store, &Application{ID: "app1", HostID: "h1", SourceType: "image", Image: "x:latest"})
	dep := &Deployment{ID: "dep1", AppID: app.ID, Status: DeployBuilding, StartedAt: time.Now()}
	if err := store.AddDeployment(dep); err != nil {
		t.Fatalf("AddDeployment: %v", err)
	}
	_, cancel := context.WithCancel(context.Background())
	if err := e.registerRunning(app.ID, &activeDeployment{depID: dep.ID, cancel: cancel}); err != nil {
		t.Fatalf("registerRunning: %v", err)
	}
	e.fail(app, dep, &logBuffer{}, "boom")
	got, ok := store.GetDeployment(app.ID, dep.ID)
	if !ok {
		t.Fatal("deployment record vanished")
	}
	if got.Status != DeployFailed {
		t.Errorf("status = %q, want %q", got.Status, DeployFailed)
	}
	if got.Error != "boom" {
		t.Errorf("error = %q, want boom", got.Error)
	}
}
