// The on-demand operator tunnel itself (`watchmanctl tunnel`): listen on a
// loopback port, and for every accepted TCP connection open one
// OperatorService.Connect stream (one stream == one connection, kubectl
// port-forward style). The stream is bridged by the control plane to an
// agent tunnel leg; no port is opened on any managed host.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"watchman/proto/agentpb"
)

const (
	// Enlarged HTTP/2 flow-control windows (mirrors the server side): the
	// 64KB defaults cap a stream near ~17 Mbps at 30ms RTT, which would
	// bottleneck tunneled throughput on anything but a LAN.
	opInitialWindowSize     = 4 << 20  // per-stream
	opInitialConnWindowSize = 16 << 20 // connection-level

	tunnelIOBuf = 32 * 1024
)

// tunnelOptions is the resolved parameter set of one `tunnel` invocation.
type tunnelOptions struct {
	AppID      string // app mode: server resolves host + checks the port allowlist
	HostID     string // host mode
	TargetHost string // empty = server resolves (loopback, or mesh IP for mesh ports)
	TargetPort uint32
	Listen     string // [ip:]port; empty = random loopback port
}

// dialFunc abstracts the data-plane dial so tests can substitute bufconn.
type dialFunc func(ctx context.Context, endpoint, token string, insecure bool) (*grpc.ClientConn, error)

// dialOperator connects to the -opgrpc listener with TLS. Transport
// credentials are mandatory: the data plane never runs in cleartext.
func dialOperator(_ context.Context, endpoint, _ string, insecure bool) (*grpc.ClientConn, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	tlsCfg := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		// Adjacent local connections each spawn a stream; session resumption
		// shaves one round trip off every subsequent handshake.
		ClientSessionCache: tls.NewLRUClientSessionCache(8),
	}
	if insecure {
		tlsCfg.InsecureSkipVerify = true
	}
	cc, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithInitialWindowSize(opInitialWindowSize),
		grpc.WithInitialConnWindowSize(opInitialConnWindowSize),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, err
	}
	return cc, nil
}

// normalizeListen resolves the --listen value to a bind address. A bare port
// ("8080") or empty host (":8080") binds loopback (kubectl semantics); an
// explicit interface like 0.0.0.0 is honored as given.
func normalizeListen(listen string) (string, error) {
	if listen == "" {
		return "127.0.0.1:0", nil
	}
	if p, err := strconv.Atoi(listen); err == nil {
		if p < 0 || p > 65535 {
			return "", fmt.Errorf("port out of range: %d", p)
		}
		return net.JoinHostPort("127.0.0.1", listen), nil
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("--listen must be [ip:]port: %v", err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return "", fmt.Errorf("invalid port %q", port)
	}
	return net.JoinHostPort(host, port), nil
}

// runTunnel serves the given local listener until ctx is cancelled (SIGINT)
// or the accept loop fails. kubectl semantics: no auto-reconnect — a tunnel
// is session state, the operator re-runs the command.
func runTunnel(ctx context.Context, lis net.Listener, cfg *config, opt tunnelOptions, info *tunnelClientInfo, dial dialFunc, out io.Writer) error {
	defer lis.Close()

	cc, err := dial(ctx, info.Endpoint, cfg.Token, cfg.Insecure)
	if err != nil {
		return err
	}
	defer cc.Close()
	client := agentpb.NewOperatorServiceClient(cc)

	fmt.Fprintf(out, "Forwarding from %s to %s\n", lis.Addr().String(), opt.describe())
	if host, _, _ := net.SplitHostPort(lis.Addr().String()); host != "127.0.0.1" && host != "::1" && host != "localhost" {
		fmt.Fprintln(out, "warning: the local listener is exposed beyond loopback; anyone who can reach it can use the tunnel")
	}
	fmt.Fprintln(out, "Press Ctrl-C to close the tunnel.")

	acceptErr := make(chan error, 1)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					acceptErr <- err
					return
				}
			}
			go proxyConn(ctx, client, cfg.Token, opt, conn, out)
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-acceptErr:
		return fmt.Errorf("accept: %w", err)
	}
}

// describe renders the tunnel target for the startup banner.
func (o tunnelOptions) describe() string {
	target := o.TargetHost
	if target == "" {
		target = "auto"
	}
	if o.AppID != "" {
		return fmt.Sprintf("app %s (port %d, via %s)", o.AppID, o.TargetPort, target)
	}
	return fmt.Sprintf("%s:%d on host %s", target, o.TargetPort, o.HostID)
}

// proxyConn bridges one accepted TCP connection to one Connect stream.
func proxyConn(ctx context.Context, client agentpb.OperatorServiceClient, token string, opt tunnelOptions, local net.Conn, out io.Writer) {
	defer local.Close()

	// Own context per connection: cancelling it (watchdog or parent) must
	// unblock stream.Recv — closing the local conn alone cannot.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	stream, err := client.Connect(metadata.AppendToOutgoingContext(streamCtx, "authorization", "Bearer "+token))
	if err != nil {
		fmt.Fprintf(out, "[%s] open stream: %v\n", local.RemoteAddr(), err)
		return
	}

	// Bound the open phase: if the service never acks, cancel the stream so
	// the Recv below returns instead of leaking the goroutine forever.
	watchdog := time.AfterFunc(15*time.Second, func() { cancelStream(); _ = local.Close() })
	defer watchdog.Stop()

	err = stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Open{Open: &agentpb.OperatorOpen{
		HostId:     opt.HostID,
		AppId:      opt.AppID,
		TargetHost: opt.TargetHost,
		TargetPort: opt.TargetPort,
	}}})
	if err == nil {
		var ackMsg *agentpb.OperatorMessage
		ackMsg, err = stream.Recv()
		if err == nil {
			ack := ackMsg.GetAck()
			switch {
			case ack == nil:
				err = fmt.Errorf("protocol error: expected ack, got %T", ackMsg.Payload)
			case !ack.GetOk():
				err = fmt.Errorf("tunnel refused: %s", ack.GetError())
			}
		}
	}
	if err != nil {
		fmt.Fprintf(out, "[%s] %v\n", local.RemoteAddr(), err)
		return
	}
	watchdog.Stop()

	// local -> stream (dedicated goroutine; Send is single-writer here).
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		buf := make([]byte, tunnelIOBuf)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				if serr := stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Data{
					Data: &agentpb.OperatorData{Data: cp},
				}}); serr != nil {
					return
				}
			}
			if err != nil {
				// Local side hung up: tell the server, unless the connection
				// is dying anyway for a reason we surfaced already.
				if err != io.EOF {
					_ = stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Close{
						Close: &agentpb.OperatorClose{Reason: "local read: " + err.Error()},
					}})
				} else {
					_ = stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Close{
						Close: &agentpb.OperatorClose{Reason: "eof"},
					}})
				}
				return
			}
		}
	}()

	// stream -> local (on this goroutine: Recv is single-reader).
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				fmt.Fprintf(out, "[%s] tunnel stream closed: %v\n", local.RemoteAddr(), err)
			}
			return
		}
		switch p := msg.Payload.(type) {
		case *agentpb.OperatorMessage_Data:
			if _, werr := local.Write(p.Data.GetData()); werr != nil {
				return
			}
		case *agentpb.OperatorMessage_Close:
			return
		default:
			// Unexpected frame mid-stream: protocol violation, tear down.
			return
		}
	}
}
