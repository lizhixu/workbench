package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"watchman/proto/agentpb"
)

func TestConfigRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg.json")
	c := &config{Server: "https://panel.example.com:18789", Token: "jwt-abc", Insecure: true}
	if err := c.saveTo(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadConfigFrom(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if *got != *c {
		t.Fatalf("round trip mismatch: %+v != %+v", got, c)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("config must be 0600, got %o", perm)
		}
	}
}

func TestLoadMissingConfigIsEmpty(t *testing.T) {
	c, err := loadConfigFrom(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if c.Server != "" || c.Token != "" {
		t.Fatalf("expected empty config, got %+v", c)
	}
}

func TestNormalizeListen(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "127.0.0.1:0"},
		{"8080", "127.0.0.1:8080"},
		{":8080", "127.0.0.1:8080"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		{"0.0.0.0:8080", "0.0.0.0:8080"},
		{"[::1]:8080", "[::1]:8080"},
	}
	for _, tc := range cases {
		got, err := normalizeListen(tc.in)
		if err != nil {
			t.Fatalf("normalizeListen(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeListen(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"65536", "-1", "1.2.3.4:99999", "host", "a:b:c"} {
		if _, err := normalizeListen(bad); err == nil {
			t.Fatalf("normalizeListen(%q) must fail", bad)
		}
	}
}

func TestParseTarget(t *testing.T) {
	host, port, err := parseTarget("6379")
	if err != nil || host != "" || port != 6379 {
		t.Fatalf("bare port: %q %d %v", host, port, err)
	}
	host, port, err = parseTarget("10.0.0.5:3306")
	if err != nil || host != "10.0.0.5" || port != 3306 {
		t.Fatalf("host:port: %q %d %v", host, port, err)
	}
	for _, bad := range []string{"", "0", "70000", ":8080", "host", "a:b"} {
		if _, _, err := parseTarget(bad); err == nil {
			t.Fatalf("parseTarget(%q) must fail", bad)
		}
	}
}

// echoOperatorService fakes the data plane: acks the open, echoes data back.
type echoOperatorService struct {
	agentpb.UnimplementedOperatorServiceServer
	gotOpen chan *agentpb.OperatorOpen
}

func (s *echoOperatorService) Connect(stream grpc.BidiStreamingServer[agentpb.OperatorMessage, agentpb.OperatorMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return fmt.Errorf("first frame must be open")
	}
	if s.gotOpen != nil {
		s.gotOpen <- open
	}
	if err := stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Ack{
		Ack: &agentpb.OperatorOpenAck{Ok: true, ConnId: "test-conn"},
	}}); err != nil {
		return err
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		switch p := msg.Payload.(type) {
		case *agentpb.OperatorMessage_Data:
			if err := stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Data{
				Data: &agentpb.OperatorData{Data: p.Data.GetData()},
			}}); err != nil {
				return err
			}
		case *agentpb.OperatorMessage_Close:
			return nil
		}
	}
}

func TestTunnelEndToEndBufconn(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fake := &echoOperatorService{gotOpen: make(chan *agentpb.OperatorOpen, 1)}
	agentpb.RegisterOperatorServiceServer(gs, fake)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	dial := func(_ context.Context, _, _ string, _ bool) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}

	localLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	runDone := make(chan error, 1)
	go func() {
		runDone <- runTunnel(ctx, localLis, &config{Token: "t"},
			tunnelOptions{HostID: "host-1", TargetPort: 6379},
			&tunnelClientInfo{Enabled: true, Endpoint: "bufnet"}, dial, &out)
	}()

	waitForOutput(t, &out, "Forwarding from")

	// Round-trip a frame through CLI -> fake service -> CLI.
	conn, err := net.DialTimeout("tcp", localLis.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial local: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("PING")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "PING" {
		t.Fatalf("echo mismatch: %q", buf)
	}

	// The open frame carried our host/port.
	select {
	case open := <-fake.gotOpen:
		if open.GetHostId() != "host-1" || open.GetTargetPort() != 6379 {
			t.Fatalf("unexpected open: %+v", open)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service never received the open frame")
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("runTunnel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runTunnel did not stop on cancel")
	}
}

// TestTunnelRefusedByServer covers the ack-ok=false path: the local
// connection is closed and a reason is printed.
func TestTunnelRefusedByServer(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	agentpb.RegisterOperatorServiceServer(gs, &refuseOperatorService{})
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	dial := func(_ context.Context, _, _ string, _ bool) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}

	localLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	runDone := make(chan error, 1)
	go func() {
		runDone <- runTunnel(ctx, localLis, &config{Token: "t"},
			tunnelOptions{HostID: "host-1", TargetPort: 22},
			&tunnelClientInfo{Enabled: true, Endpoint: "bufnet"}, dial, &out)
	}()

	conn, err := net.DialTimeout("tcp", localLis.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial local: %v", err)
	}
	// Server refuses: proxyConn closes the local conn and logs the reason.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("local connection must close when the server refuses")
	}
	conn.Close()
	waitForOutput(t, &out, "refused")
}

// syncBuffer is a race-safe io.Writer for capturing runTunnel output while
// the accept/proxy goroutines write to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitForOutput polls until the shared output buffer contains want (runs in
// the background of runTunnel, so the exact write timing is racy).
func waitForOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output never contained %q: %q", want, out.String())
}

type refuseOperatorService struct {
	agentpb.UnimplementedOperatorServiceServer
}

func (s *refuseOperatorService) Connect(stream grpc.BidiStreamingServer[agentpb.OperatorMessage, agentpb.OperatorMessage]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Ack{
		Ack: &agentpb.OperatorOpenAck{Ok: false, Error: "viewer 角色不允许使用运维隧道"},
	}})
}
