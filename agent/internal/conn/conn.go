// Package conn implements the agent's outbound connection to the control
// server: it dials the gRPC server, opens the Connect bidi stream, sends the
// RegisterRequest, starts the heartbeat loop, and reconnects with exponential
// backoff when the stream breaks.
package conn

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"runtime"
	"time"

	"watchman/agent/internal/cleanup"
	"watchman/agent/internal/config"
	"watchman/agent/internal/docker"
	"watchman/agent/internal/exec"
	"watchman/agent/internal/files"
	"watchman/agent/internal/metrics"
	"watchman/agent/internal/scan"
	"watchman/agent/internal/shell"
	"watchman/agent/internal/sysinfo"
	"watchman/agent/internal/tunnel"
	"watchman/agent/internal/uninstall"
	"watchman/agent/internal/upgrade"
	"watchman/internal/version"
	"watchman/proto/agentpb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
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
	cleanup *cleanup.Manager
	upgrade *upgrade.Manager
	scan    *scan.Manager
	tunnel  *tunnel.Manager

	uninstall *uninstall.Executor

	// connCancel tears down the current stream so the dialer reconnects
	// (used when a redirect is applied mid-session). It is only ever
	// touched from the Run goroutine that owns connectOnce.
	connCancel context.CancelFunc
	// redirectApplied is set when a redirect is applied mid-session; the
	// receive loop then reports the stream end as a planned reconnect
	// instead of a failure.
	redirectApplied bool
	// redirectFailedSince marks when the current redirect target started
	// failing continuously; cleared on a successful registration. It
	// backs the fallback window in maybeFallback.
	redirectFailedSince time.Time
}

// errRedirectReconnect is returned by connectOnce when the agent
// deliberately drops its stream to dial a redirect target. Run treats it
// as a planned reconnect (no error backoff), not a failure.
var errRedirectReconnect = errors.New("redirect: reconnecting to new server address")

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

// SetCleanupManager wires system cleanup execution.
func (d *Dialer) SetCleanupManager(m *cleanup.Manager) { d.cleanup = m }

// SetUpgradeManager wires agent self-upgrade.
func (d *Dialer) SetUpgradeManager(m *upgrade.Manager) { d.upgrade = m }

// SetScanManager wires security scanning.
func (d *Dialer) SetScanManager(m *scan.Manager) { d.scan = m }

// SetTunnelManager wires reverse TCP tunnels.
func (d *Dialer) SetTunnelManager(m *tunnel.Manager) { d.tunnel = m }

// SetUninstallExecutor wires agent self-uninstall.
func (d *Dialer) SetUninstallExecutor(e *uninstall.Executor) { d.uninstall = e }

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
		if errors.Is(err, errRedirectReconnect) {
			// Planned move to a redirect target: reconnect promptly
			// instead of sitting out the failure backoff.
			attempt = 0
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if d.maybeFallback() {
			// The redirect proved unreachable and the old server is
			// still there: we already reverted to it, reconnect now.
			attempt = 0
			continue
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

// transportCreds returns TLS credentials when cfg.TLS is set, and insecure
// credentials otherwise. TLS uses the system root CAs; TLSServerName can
// override the expected server name (useful for IP-based control servers).
func (d *Dialer) transportCreds() credentials.TransportCredentials {
	if !d.cfg.TLS {
		d.log.Warn("dialing control server without TLS; traffic is unencrypted")
		return insecure.NewCredentials()
	}
	tlsCfg := &tls.Config{ServerName: d.cfg.TLSServerName}
	return credentials.NewTLS(tlsCfg)
}

func (d *Dialer) backoff(attempt int) time.Duration {
	base := math.Pow(2, float64(attempt))
	if base > 60 {
		base = 60
	}
	return time.Duration(base*float64(time.Second)) + time.Millisecond*time.Duration(attempt*17%500)
}

// probeTCP reports whether something accepts a TCP connection at addr
// within the redirect probe timeout. It only proves the port answers,
// not that it is our control plane — the real check is the gRPC
// registration that follows the redirect.
func probeTCP(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// applyRedirectFromServer handles a redirect target pushed by the server
// (ConfigUpdate or RegisterResponse). It returns true when the agent
// switched its dial address and the caller should reconnect. An empty
// target means "no opinion" and never changes anything; an unreachable
// target is ignored and the agent stays where it is.
func (d *Dialer) applyRedirectFromServer(target string) bool {
	now := time.Now()
	target = config.NormalizeServerAddr(target)
	if target == "" {
		return false
	}
	current := config.NormalizeServerAddr(d.cfg.EffectiveAddr())
	if target == current {
		return false
	}
	if d.cfg.RedirectCoolingDown(target, now) {
		d.log.Info("redirect target in failure cooldown, ignoring", "target", target)
		return false
	}
	if d.cfg.RedirectReversal(target, now) {
		d.log.Warn("ignoring redirect back to previous server (possible redirect loop between two control planes)",
			"target", target, "current", current)
		return false
	}
	if !probeTCP(target, config.RedirectProbeTimeout) {
		d.log.Warn("redirect target unreachable, staying on current server",
			"target", target, "current", current)
		return false
	}
	if err := d.cfg.SetRedirect(target, current, now); err != nil {
		d.log.Warn("persist redirect", "err", err)
		return false
	}
	if err := d.cfg.SyncAgentConf(target); err != nil {
		d.log.Debug("sync agent.conf", "err", err)
	}
	d.redirectFailedSince = time.Time{}
	d.log.Info("redirect applied, reconnecting to new server",
		"target", target, "previous", current)
	return true
}

// maybeFallback reverts an applied redirect after it has failed
// continuously for RedirectFallbackAfter — but only when the previous
// server still answers a TCP probe. If the old control plane is already
// gone, the agent keeps dialing the new address. It returns true when
// it reverted (the caller should reconnect promptly).
func (d *Dialer) maybeFallback() bool {
	target, prev := d.cfg.RedirectState()
	if target == "" {
		d.redirectFailedSince = time.Time{}
		return false
	}
	if d.redirectFailedSince.IsZero() {
		d.redirectFailedSince = time.Now()
		return false
	}
	if time.Since(d.redirectFailedSince) < config.RedirectFallbackAfter {
		return false
	}
	prev = config.NormalizeServerAddr(prev)
	if prev == "" || !probeTCP(prev, config.RedirectProbeTimeout) {
		// Old server not reachable either: keep trying the new one.
		return false
	}
	d.log.Warn("redirect target unreachable, falling back to previous server",
		"target", target, "previous", prev)
	if err := d.cfg.NoteRedirectFailed(time.Now()); err != nil {
		d.log.Warn("persist redirect fallback", "err", err)
	}
	if err := d.cfg.SyncAgentConf(d.cfg.EffectiveAddr()); err != nil {
		d.log.Debug("sync agent.conf", "err", err)
	}
	d.redirectFailedSince = time.Time{}
	return true
}

func (d *Dialer) connectOnce(ctx context.Context) error {
	addr := config.NormalizeServerAddr(d.cfg.EffectiveAddr())
	if addr == "" {
		return fmt.Errorf("no server address configured")
	}

	kacp := keepalive.ClientParameters{
		Time:                20 * time.Second, // Send keepalive ping every 20s if no activity
		Timeout:             10 * time.Second, // Wait 10s for ping ack before tearing down connection
		PermitWithoutStream: true,             // Send keepalive pings even without active streams
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(d.transportCreds()),
		grpc.WithKeepaliveParams(kacp),
	}
	cc, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer cc.Close()

	client := agentpb.NewAgentServiceClient(cc)

	// A per-connection context so a redirect applied mid-session can
	// tear this stream down without cancelling the whole dialer.
	connCtx, connCancel := context.WithCancel(ctx)
	d.connCancel = connCancel
	defer func() { d.connCancel = nil }()

	callCtx := connCtx
	if authToken := d.cfg.Snapshot().AuthToken; authToken != "" {
		callCtx = metadata.AppendToOutgoingContext(connCtx,
			"authorization", "Bearer "+authToken)
	}

	stream, err := client.Connect(callCtx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	// Take a consistent snapshot: the upgrade callback and the maintenance
	// handler mutate the state from other goroutines.
	st := d.cfg.Snapshot()
	hwInfo := CollectHostHardwareInfo()
	reconnectReason := st.ReconnectReason
	reg := &agentpb.RegisterRequest{
		EnrollToken:     d.cfg.EnrollToken,
		AgentId:         st.AgentID,
		Hostname:        hostname(),
		Os:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		Distro:          hwInfo.Distro,
		AgentVersion:    version.Get(),
		Uptime:          hwInfo.Uptime,
		CpuCores:        hwInfo.CPUCores,
		MemTotal:        hwInfo.MemTotal,
		InternalIp:      hwInfo.InternalIP,
		PublicIp:        hwInfo.PublicIP,
		Location:        hwInfo.Location,
		ReconnectReason: reconnectReason,
		// Report the address actually being dialed (after redirect) plus
		// any outstanding redirect failure, so the panel can show which
		// address each agent uses and which moves failed.
		ServerAddr:           addr,
		RedirectFailedTarget: d.cfg.FailedTargetForReport(time.Now()),
	}
	if reconnectReason != "" {
		d.log.Info("sending register with reconnect reason", "reason", reconnectReason)
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

	// The panel's per-host traffic reset day arrives on every (re)connect;
	// applying it keeps the agent's billing cycle aligned with the panel
	// setting (a 0 from an old server is ignored).
	if d.metrics != nil {
		d.metrics.SetResetDay(int(regResp.GetTrafficResetDay()))
		d.metrics.SetProbeTargets(regResp.GetProbeTargets())
	}

	if regResp.GetAuthToken() != "" {
		newID := regResp.GetAgentId()
		if err := d.cfg.Update(func(s *config.State) {
			s.AgentID = newID
			s.AuthToken = regResp.GetAuthToken()
			// One-shot: consume the reason so a later unexpected drop
			// resumes normal alerting (AGENTS.md 8.6.4).
			s.ReconnectReason = ""
		}); err != nil {
			d.log.Warn("save state", "err", err)
		}
		d.log.Info("registered, identity persisted", "agent_id", newID)
	} else if d.cfg.ReconnectReason() != "" {
		// Already known to the server (no new token issued): still consume
		// any pending reason.
		if err := d.cfg.Update(func(s *config.State) { s.ReconnectReason = "" }); err != nil {
			d.log.Warn("clear reconnect reason", "err", err)
		}
	}

	// A redirect delivered with registration (offline agents catch up
	// here): probe it and, when it answers, drop this stream and dial
	// the new server instead of wiring subsystems to a stream we are
	// about to abandon.
	if d.applyRedirectFromServer(regResp.GetServerAddr()) {
		return errRedirectReconnect
	}
	// Registration against the current server succeeded: any redirect
	// failure window restarts from a later failure.
	d.redirectFailedSince = time.Time{}
	d.redirectApplied = false

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
	if d.cleanup != nil {
		d.cleanup.SetSender(hub)
	}
	if d.upgrade != nil {
		d.upgrade.SetSender(hub)
	}
	if d.scan != nil {
		d.scan.SetSender(hub)
	}
	if d.tunnel != nil {
		d.tunnel.SetSender(hub)
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
			if d.redirectApplied {
				// A redirect was applied mid-session (ConfigUpdate):
				// this is a planned move, not a failure.
				d.redirectApplied = false
				return errRedirectReconnect
			}
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

	// ExecCancel: the server wants a running exec stopped (the operator
	// cancelled the deployment, or the server-side wait timed out). The
	// manager kills the process group and reports "cancelled"; an
	// unknown or already-finished exec is a no-op, not an error.
	case *agentpb.ServerMessage_ExecCancel:
		if d.execs == nil {
			return fmt.Errorf("exec not available")
		}
		d.execs.Cancel(p.ExecCancel)
		return nil

	// Metrics. Answered off the receive loop: a one-shot sample collects
	// CPU/disk/network counters and must never serialise the loop behind
	// the panel's poll rate — that starved sysinfo/files/docker/terminal
	// round trips until their timeouts. The reply still travels the same
	// hub sender, so the server's metrics-live handler receives it.
	case *agentpb.ServerMessage_MetricsQ:
		if d.metrics == nil {
			return fmt.Errorf("metrics not available")
		}
		go d.metrics.Handle(p.MetricsQ)
		return nil

	// Runtime config pushed live by the server when the admin saves the
	// billing modal (probe targets / switch, traffic reset day) — same
	// semantics as what RegisterResponse would carry on the next reconnect.
	// A non-empty server_addr in the same message is a redirect: the
	// agent probes the target and moves over when it answers.
	case *agentpb.ServerMessage_ConfigUpdate:
		if d.metrics != nil {
			d.metrics.SetProbeTargets(p.ConfigUpdate.GetProbeTargets())
			d.metrics.SetResetDay(int(p.ConfigUpdate.GetTrafficResetDay()))
		}
		if d.applyRedirectFromServer(p.ConfigUpdate.GetServerAddr()) {
			d.redirectApplied = true
			if d.connCancel != nil {
				d.connCancel()
			}
		}
		return nil

	// One-time manual correction of the monthly traffic totals, pushed
	// live by the server when the admin saves the traffic-correction
	// modal. The agent sets MonthRx/MonthTx to the supplied values and
	// persists them; later samples only add deltas observed after the
	// correction.
	case *agentpb.ServerMessage_TrafficCorrect:
		if d.metrics == nil {
			return nil
		}
		d.metrics.SetTrafficCorrection(p.TrafficCorrect.GetRxBytes(), p.TrafficCorrect.GetTxBytes())
		return nil

	// SysInfo. Answered off the receive loop for the same reason as
	// MetricsQ: a slow collection kind must not stall the loop and starve
	// every other server->agent round trip. The reply still travels the
	// hub sender; the server's 10s sysinfo timeout still bounds the wait.
	case *agentpb.ServerMessage_SysinfoQ:
		if d.sysinfo == nil {
			return fmt.Errorf("sysinfo not available")
		}
		go d.sysinfo.Handle(p.SysinfoQ)
		return nil

	// Docker.
	case *agentpb.ServerMessage_DockerOp:
		if d.docker == nil {
			return fmt.Errorf("docker not available")
		}
		d.docker.Handle(p.DockerOp)
		return nil

	// System cleanup.
	case *agentpb.ServerMessage_Cleanup:
		if d.cleanup == nil {
			return fmt.Errorf("cleanup not available")
		}
		d.cleanup.Handle(p.Cleanup)
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

	// Reverse TCP tunnel.
	case *agentpb.ServerMessage_TunnelOpen:
		if d.tunnel == nil {
			return fmt.Errorf("tunnel not available")
		}
		go d.tunnel.HandleOpen(p.TunnelOpen)
		return nil
	case *agentpb.ServerMessage_TunnelData:
		if d.tunnel == nil {
			return nil
		}
		d.tunnel.HandleData(p.TunnelData)
		return nil
	case *agentpb.ServerMessage_TunnelClose:
		if d.tunnel == nil {
			return nil
		}
		d.tunnel.HandleClose(p.TunnelClose)
		return nil

	// Self-uninstall requested by the server (host unbound with
	// "uninstall agent"). Runs async, then the process exits.
	case *agentpb.ServerMessage_Uninstall:
		if d.uninstall == nil {
			return fmt.Errorf("uninstall not available")
		}
		d.uninstall.Handle(p.Uninstall)
		return nil
	// Maintenance notice from server (e.g. server restart). Persist it so the
	// reconnect after the restart carries the reason and stays quiet.
	case *agentpb.ServerMessage_Maintenance:
		m := p.Maintenance
		d.log.Info("server announced maintenance", "reason", m.GetReason(), "duration_sec", m.GetExpectedDurationSec())
		if err := d.cfg.SetReconnectReason("maintenance"); err != nil {
			d.log.Warn("persist maintenance reason", "err", err)
		}
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
			// Route through the send channel: sendPump is the only
			// goroutine allowed to call stream.Send (gRPC streams are
			// not safe for concurrent Send).
			if !hub.Send(&agentpb.AgentMessage{
				Payload: &agentpb.AgentMessage_Heartbeat{
					Heartbeat: &agentpb.Heartbeat{Ts: time.Now().Unix()},
				},
			}) {
				d.log.Debug("heartbeat dropped, send channel full")
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

// Send pushes a message to the server (thread-safe). It blocks up to
// sendTimeout waiting for channel capacity instead of silently dropping
// the message; callers get false when the hub is gone or the timeout hits.
func (h *Hub) Send(msg *agentpb.AgentMessage) bool {
	select {
	case <-h.done:
		return false
	default:
	}
	select {
	case h.sendCh <- msg:
		return true
	case <-h.done:
		return false
	case <-time.After(sendTimeout):
		return false
	}
}

const sendTimeout = 5 * time.Second

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
