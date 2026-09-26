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
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"watchman/proto/agentpb"
)

const (
	tunnelIOBuf = 32 * 1024
	// maxTunnelsPerAgent caps how many tunnels one agent may own.
	// Each tunnel holds a loopback listener, so this bounds fd usage.
	maxTunnelsPerAgent = 16
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
type tunnelConn struct {
	id     string
	sock   net.Conn
	hub    *Hub        // agent stream this leg runs on (for TunnelClose on teardown)
	toSock chan []byte // agent -> server payloads
	done   chan struct{}
	once   sync.Once
}

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
func (c *Coordinator) CloseByAgent(agentID string) {
	c.mu.Lock()
	ids := make([]string, 0)
	for id, st := range c.tunnels {
		if st.AgentID == agentID {
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	for _, id := range ids {
		_ = c.Close(id)
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
		_ = tc.sock.Close()
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
