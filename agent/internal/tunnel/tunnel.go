// Package tunnel implements the agent side of reverse TCP tunnels.
//
// A tunnel carries one TCP connection from the control server to a target
// address reachable from the agent's LAN, multiplexed over the existing
// agent<->server gRPC bidi stream:
//
//	server -> agent: TunnelOpen{tunnel_id, target_host, target_port}
//	both ways:      TunnelData{tunnel_id, data}
//	either way:     TunnelClose{tunnel_id}
//
// On TunnelOpen the agent dials target_host:target_port on its local
// network and proxies bytes between that socket and the stream.
package tunnel

import (
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"watchman/proto/agentpb"
)

// Sender delivers AgentMessages to the control server.
type Sender interface {
	Send(*agentpb.AgentMessage) bool
}

const (
	dialTimeout = 10 * time.Second
	ioBufSize   = 32 * 1024
)

// Manager tracks the agent's live tunnel legs.
type Manager struct {
	log    *slog.Logger
	sender Sender

	mu    sync.Mutex
	conns map[string]*leg
	// pending buffers payloads that arrive before the leg's dial
	// completes (TunnelOpen is handled async, TunnelData is not).
	pending map[string][][]byte
}

// leg is one proxied TCP connection.
type leg struct {
	id     string
	sock   net.Conn
	toSock chan []byte // server -> agent payloads
	done   chan struct{}
	once   sync.Once
}

// NewManager creates a tunnel manager.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, conns: make(map[string]*leg), pending: make(map[string][][]byte)}
}

// SetSender wires the stream sender (called on every reconnect).
func (m *Manager) SetSender(s Sender) {
	m.mu.Lock()
	m.sender = s
	// A new stream means old legs are dead: close them so stale
	// goroutines don't leak across reconnects.
	legs := make([]*leg, 0, len(m.conns))
	for _, l := range m.conns {
		legs = append(legs, l)
	}
	m.conns = make(map[string]*leg)
	m.mu.Unlock()
	for _, l := range legs {
		m.closeLeg(l, "reconnect")
	}
}

func (m *Manager) getSender() Sender {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sender
}

// HandleOpen dials the requested target and starts proxying.
func (m *Manager) HandleOpen(req *agentpb.TunnelOpen) {
	id := req.GetTunnelId()
	target := net.JoinHostPort(req.GetTargetHost(), strconv.Itoa(int(req.GetTargetPort())))
	sock, err := net.DialTimeout("tcp", target, dialTimeout)
	if err != nil {
		m.log.Warn("tunnel dial failed", "tunnel_id", id, "target", target, "err", err)
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		// Tell the server the leg is dead so it can fail fast.
		if s := m.getSender(); s != nil {
			s.Send(&agentpb.AgentMessage{Payload: &agentpb.AgentMessage_TunnelClose{
				TunnelClose: &agentpb.TunnelClose{TunnelId: id},
			}})
		}
		return
	}
	l := &leg{id: id, sock: sock, toSock: make(chan []byte, 64), done: make(chan struct{})}
	m.mu.Lock()
	m.conns[id] = l
	pend := m.pending[id]
	delete(m.pending, id)
	m.mu.Unlock()
	m.log.Info("tunnel leg opened", "tunnel_id", id, "target", target)
	// Drain payloads that arrived while the dial was in flight.
	for _, data := range pend {
		select {
		case l.toSock <- data:
		case <-l.done:
		default:
		}
	}
	go m.sockToStream(l)
	go m.streamToSock(l)
}

// HandleData forwards a server payload to the tunnel leg's socket.
// Never blocks the stream recv loop: on a slow consumer the leg is
// dropped instead of stalling every other channel. Payloads that arrive
// before the async dial completes are buffered (bounded) and drained by
// HandleOpen once the leg is registered.
func (m *Manager) HandleData(d *agentpb.TunnelData) {
	m.mu.Lock()
	l := m.conns[d.GetTunnelId()]
	if l == nil {
		if pend := m.pending[d.GetTunnelId()]; len(pend) < 64 {
			cp := append([]byte(nil), d.GetData()...)
			m.pending[d.GetTunnelId()] = append(pend, cp)
		}
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	select {
	case l.toSock <- d.GetData():
	case <-l.done:
	default:
		m.log.Warn("tunnel leg slow consumer, dropping", "tunnel_id", d.GetTunnelId())
		m.dropLeg(d.GetTunnelId())
	}
}

// HandleClose tears down a tunnel leg.
func (m *Manager) HandleClose(c *agentpb.TunnelClose) {
	m.mu.Lock()
	l := m.conns[c.GetTunnelId()]
	if l != nil {
		delete(m.conns, c.GetTunnelId())
	}
	delete(m.pending, c.GetTunnelId())
	m.mu.Unlock()
	if l != nil {
		m.closeLeg(l, "server closed")
	}
}

// sockToStream pumps socket bytes into the gRPC stream.
func (m *Manager) sockToStream(l *leg) {
	defer m.dropLeg(l.id)
	buf := make([]byte, ioBufSize)
	for {
		n, err := l.sock.Read(buf)
		if n > 0 {
			cp := make([]byte, n)
			copy(cp, buf[:n])
			s := m.getSender()
			if s == nil || !s.Send(&agentpb.AgentMessage{Payload: &agentpb.AgentMessage_TunnelData{
				TunnelData: &agentpb.TunnelData{TunnelId: l.id, Data: cp},
			}}) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// streamToSock pumps stream payloads into the socket.
func (m *Manager) streamToSock(l *leg) {
	defer m.dropLeg(l.id)
	for {
		select {
		case <-l.done:
			return
		case data := <-l.toSock:
			if len(data) == 0 {
				return
			}
			if _, err := l.sock.Write(data); err != nil {
				return
			}
		}
	}
}

// dropLeg removes and closes a leg (idempotent).
func (m *Manager) dropLeg(id string) {
	m.mu.Lock()
	l := m.conns[id]
	if l != nil {
		delete(m.conns, id)
	}
	delete(m.pending, id)
	m.mu.Unlock()
	if l != nil {
		m.closeLeg(l, "eof/error")
	}
}

func (m *Manager) closeLeg(l *leg, reason string) {
	l.once.Do(func() {
		close(l.done)
		_ = l.sock.Close()
		if s := m.getSender(); s != nil {
			s.Send(&agentpb.AgentMessage{Payload: &agentpb.AgentMessage_TunnelClose{
				TunnelClose: &agentpb.TunnelClose{TunnelId: l.id},
			}})
		}
		m.log.Info("tunnel leg closed", "tunnel_id", l.id, "reason", reason)
	})
}
