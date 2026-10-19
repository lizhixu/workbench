// Reverse TCP tunnels (dynamic networking, §3.9).
//
// A Tunnel is a port-forward: the control server listens on a loopback
// port and forwards every accepted TCP connection through an agent to a
// target address on the agent's network. Bytes travel multiplexed over
// the existing agent<->server gRPC bidi stream:
//
//	server -> agent: TunnelOpen{tunnel_id, target_host, target_port}
//	both ways:      TunnelData{tunnel_id, data}
//	either way:     TunnelClose{tunnel_id}
//
// Each accepted TCP connection becomes one wire tunnel leg (unique
// tunnel_id). Tunnels do not survive agent reconnects: legs opened on an
// old stream are closed when the agent re-registers.
package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"watchman/proto/agentpb"
)

var (
	// ErrOwnerLegsExceeded marks a per-owner leg cap hit: a policy denial
	// (caller audits op_tunnel_deny), not an infrastructure failure.
	ErrOwnerLegsExceeded = errors.New("too many operator tunnel legs for this user")
	// ErrAgentLegsExceeded marks a per-agent stream-leg cap hit (policy denial).
	ErrAgentLegsExceeded = errors.New("too many tunnel legs on agent")
)

const (
	tunnelIOBuf = 32 * 1024
	// maxTunnelsPerAgent caps how many tunnels one agent may own.
	// Each tunnel holds a loopback listener, so this bounds fd usage.
	maxTunnelsPerAgent = 16
	// maxStreamLegsPerAgent caps listener-less operator stream legs per agent,
	// bounding memory (each leg holds up to 64 32KiB payloads) on top of the
	// agent-side maxLegs enforcement.
	maxStreamLegsPerAgent = 64

	// operatorLegIdleTimeout reclaims an operator stream leg after this long
	// without any data flowing in either direction.
	operatorLegIdleTimeout = 15 * time.Minute
	// operatorLegMaxLifetime hard-caps an operator stream leg regardless of
	// activity; users re-open tunnels rather than keeping them for days.
	operatorLegMaxLifetime = 8 * time.Hour
)

// Tunnel describes one reverse port-forward.
type Tunnel struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agent_id"`
	TargetHost string    `json:"target_host"`
	TargetPort uint32    `json:"target_port"`
	LocalPort  int       `json:"local_port"`
	Created    time.Time `json:"created_at"`
}

// tunnelConn is one live TCP connection proxied through a tunnel leg.
// sock is nil for stream legs created via OpenLeg (operator tunnels): their
// data plane is the caller's own gRPC stream draining toSock, not a local
// TCP socket, so only done/toSock/hub are meaningful there.
type tunnelConn struct {
	id     string
	sock   net.Conn
	hub    *Hub        // agent stream this leg runs on (for TunnelClose on teardown)
	toSock chan []byte // agent -> server payloads
	done   chan struct{}
	once   sync.Once

	// Stream-leg (operator tunnel) bookkeeping. owner == "" marks a classic
	// loopback-listener leg; non-empty legs belong to the operator service and
	// are subject to the idle/lifetime reaper (StartReaper).
	owner      string
	desc       string // human-readable target, e.g. "127.0.0.1:6379" (audit/UI)
	createdAt  time.Time
	lastActive atomic.Int64 // unix nano, touched on data flow both ways
}

func (tc *tunnelConn) touch() { tc.lastActive.Store(time.Now().UnixNano()) }

type tunnelState struct {
	Tunnel
	listener net.Listener
	hub      *Hub
	conns    map[string]*tunnelConn
	closed   bool
}

// Coordinator manages reverse TCP tunnels.
type Coordinator struct {
	reg *Registry
	log *slog.Logger

	mu      sync.Mutex
	tunnels map[string]*tunnelState
	// legs routes wire tunnel_ids to live connections (globally unique ids).
	legs map[string]*tunnelConn
}

// NewTunnelCoordinator creates a tunnel coordinator bound to a registry.
func NewTunnelCoordinator(reg *Registry, log *slog.Logger) *Coordinator {
	if log == nil {
		log = slog.Default()
	}
	return &Coordinator{
		reg:     reg,
		log:     log,
		tunnels: make(map[string]*tunnelState),
		legs:    make(map[string]*tunnelConn),
	}
}

// SetTunnelCoordinator installs the coordinator on the registry so
// agent tunnel messages are dispatched to it.
func (r *Registry) SetTunnelCoordinator(c *Coordinator) {
	r.mu.Lock()
	r.tunnel = c
	r.mu.Unlock()
}

// TunnelCoordinator returns the installed tunnel coordinator (nil if disabled).
func (r *Registry) TunnelCoordinator() *Coordinator {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tunnel
}

// Open creates a tunnel: listen on a loopback port and forward to
// targetHost:targetPort via the agent. Returns the tunnel descriptor;
// connect to 127.0.0.1:LocalPort to use it.
func (c *Coordinator) Open(agentID, targetHost string, targetPort uint32) (*Tunnel, error) {
	if targetHost == "" || targetPort == 0 {
		return nil, fmt.Errorf("target_host and target_port are required")
	}
	hub := c.reg.Hub(agentID)
	if hub == nil {
		return nil, fmt.Errorf("agent %q offline", agentID)
	}
	c.mu.Lock()
	n := 0
	for _, st := range c.tunnels {
		if st.AgentID == agentID {
			n++
		}
	}
	c.mu.Unlock()
	if n >= maxTunnelsPerAgent {
		return nil, fmt.Errorf("agent %q already has %d tunnels (max)", agentID, maxTunnelsPerAgent)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	st := &tunnelState{
		Tunnel: Tunnel{
			ID:         randomToken(12),
			AgentID:    agentID,
			TargetHost: targetHost,
			TargetPort: targetPort,
			LocalPort:  ln.Addr().(*net.TCPAddr).Port,
			Created:    time.Now(),
		},
		listener: ln,
		hub:      hub,
		conns:    make(map[string]*tunnelConn),
	}
	c.mu.Lock()
	c.tunnels[st.ID] = st
	c.mu.Unlock()
	c.log.Info("tunnel opened", "tunnel_id", st.ID, "agent_id", agentID,
		"target", fmt.Sprintf("%s:%d", targetHost, targetPort), "local_port", st.LocalPort)
	go c.acceptLoop(st)
	return &st.Tunnel, nil
}

// List returns all open tunnels.
func (c *Coordinator) List() []Tunnel {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Tunnel, 0, len(c.tunnels))
	for _, st := range c.tunnels {
		out = append(out, st.Tunnel)
	}
	return out
}

// Get returns the tunnel with the given ID, or false if not found.
func (c *Coordinator) Get(id string) (Tunnel, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.tunnels[id]
	if st == nil {
		return Tunnel{}, false
	}
	return st.Tunnel, true
}

// Close shuts down a tunnel and all its connections.
func (c *Coordinator) Close(id string) error {
	c.mu.Lock()
	st := c.tunnels[id]
	if st == nil {
		c.mu.Unlock()
		return fmt.Errorf("tunnel %q not found", id)
	}
	delete(c.tunnels, id)
	st.closed = true
	conns := make([]*tunnelConn, 0, len(st.conns))
	for _, tc := range st.conns {
		conns = append(conns, tc)
	}
	for _, tc := range conns {
		delete(c.legs, tc.id)
	}
	c.mu.Unlock()
	_ = st.listener.Close()
	for _, tc := range conns {
		c.closeLeg(tc, "tunnel closed")
	}
	c.log.Info("tunnel closed", "tunnel_id", id)
	return nil
}

// CloseByAgent shuts down every tunnel owned by agentID. Called when the
// agent (re-)registers: legs are bound to the old stream's hub, so they
// can never work again — drop them instead of leaking listeners.
// Orphan stream legs (operator tunnels, no owning tunnelState) die the same
// way; they only exist in the legs routing table.
func (c *Coordinator) CloseByAgent(agentID string) {
	c.mu.Lock()
	ids := make([]string, 0)
	for id, st := range c.tunnels {
		if st.AgentID == agentID {
			ids = append(ids, id)
		}
	}
	orphanLegs := make([]string, 0)
	for id, tc := range c.legs {
		if tc.hub == nil || tc.hub.AgentID != agentID || tc.owner == "" {
			continue
		}
		orphanLegs = append(orphanLegs, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		_ = c.Close(id)
	}
	for _, id := range orphanLegs {
		c.dropLeg(id)
	}
}

// OpenLeg creates a listener-less tunnel leg: bytes flow between the agent
// (via TunnelData frames carrying connID) and the returned inbound channel,
// which the caller (operator service) pumps into its own gRPC stream. The
// leg is tracked in the legs routing table only; dropLeg/CloseOwnerLeg and
// the agent-side TunnelClose all tear it down through the usual paths.
// ownerLimit > 0 additionally caps live legs of that owner; both caps are
// checked and the leg inserted under one mutex hold (no count-then-insert
// race). Cap hits wrap ErrOwnerLegsExceeded / ErrAgentLegsExceeded.
func (c *Coordinator) OpenLeg(hub *Hub, targetHost string, targetPort uint32, owner, desc string, ownerLimit int) (string, <-chan []byte, error) {
	if hub == nil {
		return "", nil, fmt.Errorf("agent offline")
	}
	if targetHost == "" || targetPort == 0 {
		return "", nil, fmt.Errorf("target host and port are required")
	}

	tc := &tunnelConn{
		id:        randomToken(12),
		hub:       hub,
		toSock:    make(chan []byte, 64),
		done:      make(chan struct{}),
		owner:     owner,
		desc:      desc,
		createdAt: time.Now(),
	}
	tc.touch()

	c.mu.Lock()
	agentLegs, ownerLegs := 0, 0
	for _, l := range c.legs {
		if l.hub != nil && l.hub.AgentID == hub.AgentID {
			agentLegs++
		}
		if ownerLimit > 0 && l.sock == nil && l.owner == owner {
			ownerLegs++
		}
	}
	if agentLegs >= maxStreamLegsPerAgent {
		c.mu.Unlock()
		return "", nil, fmt.Errorf("%w: agent %q already carries %d legs", ErrAgentLegsExceeded, hub.AgentID, maxStreamLegsPerAgent)
	}
	if ownerLimit > 0 && ownerLegs >= ownerLimit {
		c.mu.Unlock()
		return "", nil, fmt.Errorf("%w: 同一用户最多同时打开 %d 条运维隧道", ErrOwnerLegsExceeded, ownerLimit)
	}
	if _, taken := c.legs[tc.id]; taken {
		c.mu.Unlock()
		return "", nil, fmt.Errorf("leg id collision")
	}
	c.legs[tc.id] = tc
	c.mu.Unlock()

	ok := hub.Send(&agentpb.ServerMessage{Payload: &agentpb.ServerMessage_TunnelOpen{
		TunnelOpen: &agentpb.TunnelOpen{
			TunnelId:   tc.id,
			TargetHost: targetHost,
			TargetPort: targetPort,
		},
	}})
	if !ok {
		c.dropLeg(tc.id)
		return "", nil, fmt.Errorf("agent stream unavailable")
	}
	return tc.id, tc.toSock, nil
}

// legByID returns the live leg registered under id, or nil. Same-package
// accessor for the operator service, which watches leg.done to learn about
// teardowns initiated from the agent side.
func (c *Coordinator) legByID(id string) *tunnelConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.legs[id]
}

// TouchLeg records data activity on an operator stream leg so the idle
// reaper does not reclaim live tunnels.
func (c *Coordinator) TouchLeg(id string) {
	c.mu.Lock()
	tc := c.legs[id]
	c.mu.Unlock()
	if tc != nil {
		tc.touch()
	}
}

// OwnerLeg describes one operator's live stream leg for the REST listing.
type OwnerLeg struct {
	ConnID      string    `json:"conn_id"`
	AgentID     string    `json:"agent_id"`
	Target      string    `json:"target"`
	CreatedAt   time.Time `json:"created_at"`
	IdleSeconds int64     `json:"idle_seconds"`
}

// ListOwnerLegs returns the live operator stream legs owned by username.
func (c *Coordinator) ListOwnerLegs(owner string) []OwnerLeg {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	out := make([]OwnerLeg, 0)
	for _, tc := range c.legs {
		if tc.owner == "" || tc.owner != owner {
			continue
		}
		idle := int64(now.Sub(time.Unix(0, tc.lastActive.Load())).Seconds())
		out = append(out, OwnerLeg{
			ConnID:      tc.id,
			AgentID:     tc.hub.AgentID,
			Target:      tc.desc,
			CreatedAt:   tc.createdAt,
			IdleSeconds: idle,
		})
	}
	return out
}

// CloseOwnerLeg force-closes a stream leg, but only when it belongs to the
// requesting operator (or the caller is an admin).
func (c *Coordinator) CloseOwnerLeg(id, username string, isAdmin bool) error {
	c.mu.Lock()
	tc := c.legs[id]
	c.mu.Unlock()
	if tc == nil || tc.owner == "" {
		return fmt.Errorf("tunnel %q not found", id)
	}
	if !isAdmin && tc.owner != username {
		return fmt.Errorf("forbidden: not your tunnel")
	}
	c.dropLeg(id)
	return nil
}

// StartReaper periodically reclaims operator stream legs that have been idle
// beyond operatorLegIdleTimeout or alive beyond operatorLegMaxLifetime.
// Classic listener tunnels are unaffected.
func (c *Coordinator) StartReaper(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.reapStaleLegs(now)
		}
	}
}

// reapStaleLegs drops operator stream legs idle beyond operatorLegIdleTimeout
// or alive beyond operatorLegMaxLifetime. Split from StartReaper so tests can
// invoke one sweep deterministically.
func (c *Coordinator) reapStaleLegs(now time.Time) {
	var stale []string
	c.mu.Lock()
	for id, tc := range c.legs {
		if tc.owner == "" {
			continue
		}
		idle := now.Sub(time.Unix(0, tc.lastActive.Load()))
		alive := now.Sub(tc.createdAt)
		if idle > operatorLegIdleTimeout || alive > operatorLegMaxLifetime {
			stale = append(stale, id)
		}
	}
	c.mu.Unlock()
	for _, id := range stale {
		c.log.Info("operator tunnel leg reclaimed", "tunnel_id", id)
		c.dropLeg(id)
	}
}

// acceptLoop proxies each inbound TCP connection through a new tunnel leg.
func (c *Coordinator) acceptLoop(st *tunnelState) {
	for {
		sock, err := st.listener.Accept()
		if err != nil {
			return // listener closed
		}
		tc := &tunnelConn{
			id:     randomToken(12),
			sock:   sock,
			toSock: make(chan []byte, 64),
			done:   make(chan struct{}),
		}
		c.mu.Lock()
		if st.closed {
			c.mu.Unlock()
			_ = sock.Close()
			continue
		}
		st.conns[tc.id] = tc
		c.legs[tc.id] = tc
		hub := st.hub
		tc.hub = hub
		c.mu.Unlock()

		ok := hub.Send(&agentpb.ServerMessage{Payload: &agentpb.ServerMessage_TunnelOpen{
			TunnelOpen: &agentpb.TunnelOpen{
				TunnelId:   tc.id,
				TargetHost: st.TargetHost,
				TargetPort: st.TargetPort,
			},
		}})
		if !ok {
			c.dropLeg(tc.id)
			continue
		}
		go c.sockToStream(st, tc)
		go c.streamToSock(st, tc)
	}
}

// sockToStream pumps local TCP bytes into the gRPC stream.
func (c *Coordinator) sockToStream(st *tunnelState, tc *tunnelConn) {
	defer c.dropLeg(tc.id)
	buf := make([]byte, tunnelIOBuf)
	for {
		n, err := tc.sock.Read(buf)
		if n > 0 {
			cp := make([]byte, n)
			copy(cp, buf[:n])
			if !st.hub.Send(&agentpb.ServerMessage{Payload: &agentpb.ServerMessage_TunnelData{
				TunnelData: &agentpb.TunnelData{TunnelId: tc.id, Data: cp},
			}}) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// streamToSock pumps agent payloads into the local TCP socket.
func (c *Coordinator) streamToSock(st *tunnelState, tc *tunnelConn) {
	defer c.dropLeg(tc.id)
	for {
		select {
		case <-tc.done:
			return
		case data := <-tc.toSock:
			if len(data) == 0 {
				return
			}
			if _, err := tc.sock.Write(data); err != nil {
				return
			}
		}
	}
}

// handleData routes an agent TunnelData to its connection.
// Never blocks the stream recv loop: if the leg can't keep up, the leg
// is torn down (fail fast) instead of stalling every other channel.
func (c *Coordinator) handleData(d *agentpb.TunnelData) {
	c.mu.Lock()
	tc := c.legs[d.GetTunnelId()]
	c.mu.Unlock()
	if tc == nil {
		return
	}
	tc.touch()
	select {
	case tc.toSock <- d.GetData():
	case <-tc.done:
	default:
		c.log.Warn("tunnel leg slow consumer, dropping", "tunnel_id", d.GetTunnelId())
		c.dropLeg(d.GetTunnelId())
	}
}

// handleClose tears down the leg the agent closed.
func (c *Coordinator) handleClose(cl *agentpb.TunnelClose) {
	c.dropLeg(cl.GetTunnelId())
}

// dropLeg removes and closes a leg (idempotent).
func (c *Coordinator) dropLeg(id string) {
	c.mu.Lock()
	tc := c.legs[id]
	if tc != nil {
		delete(c.legs, id)
	}
	// Also detach from its tunnel state.
	for _, st := range c.tunnels {
		delete(st.conns, id)
	}
	c.mu.Unlock()
	if tc != nil {
		c.closeLeg(tc, "eof/error")
	}
}

func (c *Coordinator) closeLeg(tc *tunnelConn, reason string) {
	tc.once.Do(func() {
		close(tc.done)
		if tc.sock != nil {
			_ = tc.sock.Close()
		}
		// Tell the agent the leg is gone so it tears down its side too
		// (dialed socket + pump goroutines). Best-effort: the agent already
		// drops unknown/closed legs on its own.
		if tc.hub != nil {
			tc.hub.Send(&agentpb.ServerMessage{Payload: &agentpb.ServerMessage_TunnelClose{
				TunnelClose: &agentpb.TunnelClose{TunnelId: tc.id},
			}})
		}
		c.log.Debug("tunnel leg closed", "tunnel_id", tc.id, "reason", reason)
	})
}
