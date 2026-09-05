// Package conn implements the agent's outbound connection to the control
// server: it dials the gRPC server, opens the Connect bidi stream, sends the
// RegisterRequest, starts the heartbeat loop, and reconnects with exponential
// backoff when the stream breaks.
package conn

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"watchman/agent/internal/config"
	"watchman/agent/internal/docker"
	"watchman/agent/internal/exec"
	"watchman/agent/internal/files"
	"watchman/agent/internal/metrics"
	"watchman/agent/internal/scan"
	"watchman/agent/internal/shell"
	"watchman/agent/internal/sysinfo"
	"watchman/agent/internal/upgrade"
	"watchman/proto/agentpb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Dialer owns the lifecycle of the connection to the server.
type Dialer struct {
	cfg     *config.Config
	log     *slog.Logger
	shells  *shell.Manager
	files   *files.Manager
	execs   *exec.Manager
	metrics *metrics.Manager
	sysinfo *sysinfo.Manager
	docker  *docker.Manager
	upgrade *upgrade.Manager
	scan    *scan.Manager
}

// Hub is the agent-side view of an established stream.
type Hub struct {
	stream agentpb.AgentService_ConnectClient
	sendCh chan *agentpb.AgentMessage
	done   chan struct{}
}

// NewDialer creates a Dialer. cfg.State is mutated as registration proceeds.
func NewDialer(cfg *config.Config, log *slog.Logger) *Dialer {
	if log == nil {
		log = slog.Default()
	}
	return &Dialer{cfg: cfg, log: log}
}

// SetShellManager wires terminal session handling.
func (d *Dialer) SetShellManager(m *shell.Manager) { d.shells = m }

// SetFileManager wires file operations.
func (d *Dialer) SetFileManager(m *files.Manager) { d.files = m }

// SetExecManager wires command execution.
func (d *Dialer) SetExecManager(m *exec.Manager) { d.execs = m }

// SetMetricsManager wires metrics sampling.
func (d *Dialer) SetMetricsManager(m *metrics.Manager) { d.metrics = m }

// SetSysInfoManager wires system info collection.
func (d *Dialer) SetSysInfoManager(m *sysinfo.Manager) { d.sysinfo = m }

// SetDockerManager wires Docker operations.
func (d *Dialer) SetDockerManager(m *docker.Manager) { d.docker = m }

// SetUpgradeManager wires agent self-upgrade.
func (d *Dialer) SetUpgradeManager(m *upgrade.Manager) { d.upgrade = m }

// SetScanManager wires security scanning.
func (d *Dialer) SetScanManager(m *scan.Manager) { d.scan = m }

// Run dials and maintains the connection forever (until ctx is cancelled).
func (d *Dialer) Run(ctx context.Context) {
	var attempt int
	for {
		if ctx.Err() != nil {
			return
		}
		err := d.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		backoff := d.backoff(attempt)
		d.log.Warn("connection lost, retrying",
			"err", err, "attempt", attempt, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		attempt++
	}
}

func (d *Dialer) backoff(attempt int) time.Duration {
	base := math.Pow(2, float64(attempt))
	if base > 60 {
		base = 60
	}
	return time.Duration(base*float64(time.Second)) + time.Millisecond*time.Duration(attempt*17%500)
}

func (d *Dialer) connectOnce(ctx context.Context) error {
	addr := d.cfg.ServerAddr
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	if idx := strings.Index(addr, "/"); idx != -1 {
		addr = addr[:idx]
	}
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, "9090")
	}

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	cc, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer cc.Close()

	client := agentpb.NewAgentServiceClient(cc)

	callCtx := ctx
	if d.cfg.State.AuthToken != "" {
		callCtx = metadata.AppendToOutgoingContext(ctx,
			"authorization", "Bearer "+d.cfg.State.AuthToken)
	}

	stream, err := client.Connect(callCtx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	hwInfo := CollectHostHardwareInfo()
	reg := &agentpb.RegisterRequest{
		EnrollToken:  d.cfg.EnrollToken,
		AgentId:      d.cfg.State.AgentID,
		Hostname:     hostname(),
		Os:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Distro:       hwInfo.Distro,
		AgentVersion: "0.1.0-dev",
		Uptime:       hwInfo.Uptime,
		CpuCores:     hwInfo.CPUCores,
		MemTotal:     hwInfo.MemTotal,
		InternalIp:   hwInfo.InternalIP,
		PublicIp:     hwInfo.PublicIP,
		Location:     hwInfo.Location,
	}
	if err := stream.Send(&agentpb.AgentMessage{
		Payload: &agentpb.AgentMessage_Register{Register: reg},
	}); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv register: %w", err)
	}
	regResp := resp.GetRegister()
	if regResp == nil || !regResp.GetOk() {
		msg := "registration rejected"
		if regResp != nil {
			msg = regResp.GetError()
		}
		return fmt.Errorf("%s", msg)
	}

	if regResp.GetAuthToken() != "" {
		d.cfg.State.AgentID = regResp.GetAgentId()
		d.cfg.State.AuthToken = regResp.GetAuthToken()
		if err := d.cfg.Save(); err != nil {
			d.log.Warn("save state", "err", err)
		}
		d.log.Info("registered, identity persisted", "agent_id", d.cfg.State.AgentID)
	}

	hub := &Hub{
		stream: stream,
		sendCh: make(chan *agentpb.AgentMessage, 128),
		done:   make(chan struct{}),
	}
	defer close(hub.done)

	// Wire all subsystems to this stream.
	if d.shells != nil {
		d.shells.SetSender(hub)
		defer d.shells.CloseAll()
	}
	if d.files != nil {
		d.files.SetSender(hub)
		defer d.files.CloseAll()
	}
	if d.execs != nil {
		d.execs.SetSender(hub)
	}
	if d.metrics != nil {
		d.metrics.SetSender(hub)
		d.metrics.StartBackground(30)
		defer d.metrics.Stop()
	}
	if d.sysinfo != nil {
		d.sysinfo.SetSender(hub)
	}
	if d.docker != nil {
		d.docker.SetSender(hub)
	}
	if d.upgrade != nil {
		d.upgrade.SetSender(hub)
	}
	if d.scan != nil {
		d.scan.SetSender(hub)
	}

	heartbeatSec := regResp.GetHeartbeatIntervalSec()
	if heartbeatSec <= 0 {
		heartbeatSec = 30
	}
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go d.heartbeatLoop(hbCtx, hub, time.Duration(heartbeatSec)*time.Second)
	go d.sendPump(hub)

	for {
		msg, err := stream.Recv()
		if err != nil {
			d.log.Debug("recv ended", "err", err)
			return err
		}
		if err := d.handleServerMessage(msg); err != nil {
			d.log.Warn("handle server message", "err", err)
		}
	}
}

// handleServerMessage dispatches a single server-pushed message to the
// appropriate subsystem.
func (d *Dialer) handleServerMessage(msg *agentpb.ServerMessage) error {
	switch p := msg.Payload.(type) {
	case *agentpb.ServerMessage_Heartbeat:
		return nil

	// Terminal.
	case *agentpb.ServerMessage_TermOpen:
		if d.shells == nil {
			return fmt.Errorf("terminal not available")
		}
		return d.shells.Open(p.TermOpen)
	case *agentpb.ServerMessage_TermIn:
		if d.shells == nil {
			return nil
		}
		return d.shells.Input(p.TermIn)
	case *agentpb.ServerMessage_TermResize:
		if d.shells == nil {
			return nil
		}
		return d.shells.Resize(p.TermResize)
	case *agentpb.ServerMessage_TermClose:
		if d.shells == nil {
			return nil
		}
		go d.shells.Close(p.TermClose)
		return nil

	// Files.
	case *agentpb.ServerMessage_FileOp:
		if d.files == nil {
			return fmt.Errorf("file manager not available")
		}
		d.files.HandleAsync(p.FileOp)
		return nil

	// Exec.
	case *agentpb.ServerMessage_Exec:
		if d.execs == nil {
			return fmt.Errorf("exec not available")
		}
		d.execs.Handle(p.Exec)
		return nil

	// Metrics.
	case *agentpb.ServerMessage_MetricsQ:
		if d.metrics == nil {
			return fmt.Errorf("metrics not available")
		}
		d.metrics.Handle(p.MetricsQ)
		return nil

	// SysInfo.
	case *agentpb.ServerMessage_SysinfoQ:
		if d.sysinfo == nil {
			return fmt.Errorf("sysinfo not available")
		}
		d.sysinfo.Handle(p.SysinfoQ)
		return nil

	// Docker.
	case *agentpb.ServerMessage_DockerOp:
		if d.docker == nil {
			return fmt.Errorf("docker not available")
		}
		d.docker.Handle(p.DockerOp)
		return nil

	// Upgrade.
	case *agentpb.ServerMessage_Upgrade:
		if d.upgrade == nil {
			return fmt.Errorf("upgrade not available")
		}
		d.upgrade.Handle(p.Upgrade)
		return nil

	// Security scan.
	case *agentpb.ServerMessage_Scan:
		if d.scan == nil {
			return fmt.Errorf("scan not available")
		}
		d.scan.Handle(p.Scan)
		return nil

	default:
		d.log.Debug("server message", "type", fmt.Sprintf("%T", msg.GetPayload()))
		return nil
	}
}

func (d *Dialer) heartbeatLoop(ctx context.Context, hub *Hub, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hub.done:
			return
		case <-t.C:
			if err := hub.stream.Send(&agentpb.AgentMessage{
				Payload: &agentpb.AgentMessage_Heartbeat{
					Heartbeat: &agentpb.Heartbeat{Ts: time.Now().Unix()},
				},
			}); err != nil {
				d.log.Debug("heartbeat send failed", "err", err)
				return
			}
		}
	}
}

func (d *Dialer) sendPump(hub *Hub) {
	for {
		select {
		case <-hub.done:
			return
		case msg := <-hub.sendCh:
			if err := hub.stream.Send(msg); err != nil {
				d.log.Debug("send failed", "err", err)
				return
			}
		}
	}
}

// Send pushes a message to the server (thread-safe, best-effort).
func (h *Hub) Send(msg *agentpb.AgentMessage) bool {
	select {
	case h.sendCh <- msg:
		return true
	default:
		return false
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
