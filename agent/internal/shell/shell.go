// Package shell manages local terminal sessions on the agent.
//
// It owns a map of session_id -> *session, where each session wraps a PTY.
// Incoming TerminalOpen/Input/Resize/Close messages from the server drive the
// sessions; PTY output is streamed back as TerminalOutput messages via the
// provided Sender.
package shell

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"watchman/agent/internal/pty"
	"watchman/agent/internal/record"
	"watchman/proto/agentpb"
)

// Sender is anything that can push an agent->server message. conn.Hub implements it.
type Sender interface {
	Send(msg *agentpb.AgentMessage) bool
}

// Manager owns all terminal sessions for this agent.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*session
	sender   Sender
	log      *slog.Logger
	recordDir string // directory for asciinema cast files
}

type session struct {
	id       string
	pty      pty.PTY
	stop     chan struct{}
	recorder *record.Recorder
}

// NewManager creates an empty session manager. SetSender must be called once
// the connection is established so output can be streamed back.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	dir := filepath.Join(os.TempDir(), "watchman-records")
	// 0700: recordings may contain sensitive terminal output (passwords etc.).
	_ = os.MkdirAll(dir, 0o700)
	// Tighten permissions on dirs created by older versions (0755).
	if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm() != 0o700 {
		_ = os.Chmod(dir, 0o700)
	}
	return &Manager{
		sessions:  make(map[string]*session),
		log:       log,
		recordDir: dir,
	}
}

// SetSender wires the manager to the live connection hub.
func (m *Manager) SetSender(s Sender) {
	m.mu.Lock()
	m.sender = s
	m.mu.Unlock()
}

// Open starts a new terminal session per the TerminalOpen request.
func (m *Manager) Open(req *agentpb.TerminalOpen) error {
	cols := uint16(req.GetCols())
	rows := uint16(req.GetRows())
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	p, err := pty.Open(pty.Config{
		Shell:   req.GetShell(),
		Cwd:     req.GetCwd(),
		Cols:    cols,
		Rows:    rows,
		Account: req.GetAccount(),
	})
	if err != nil {
		return fmt.Errorf("open pty: %w", err)
	}

	s := &session{id: req.GetSessionId(), pty: p, stop: make(chan struct{})}

	// Start recording (asciinema cast v2).
	castPath := filepath.Join(m.recordDir, req.GetSessionId()+".cast")
	rec, err := record.New(castPath, int(cols), int(rows))
	if err != nil {
		m.log.Warn("start recording", "session", s.id, "err", err)
	} else {
		s.recorder = rec
	}

	m.mu.Lock()
	m.sessions[req.GetSessionId()] = s
	sender := m.sender
	m.mu.Unlock()

	// Pump PTY output -> server as TerminalOutput (and record).
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				if s.recorder != nil {
					_ = s.recorder.WriteOutput(data)
				}
				if sender != nil {
					sender.Send(&agentpb.AgentMessage{
						Payload: &agentpb.AgentMessage_TermOut{
							TermOut: &agentpb.TerminalOutput{
								SessionId: s.id,
								Data:      data,
							},
						},
					})
				}
			}
			if err != nil {
				if err != io.EOF {
					m.log.Debug("pty read end", "session", s.id, "err", err)
				}
				// Notify server the session ended.
				if sender != nil {
					sender.Send(&agentpb.AgentMessage{
						Payload: &agentpb.AgentMessage_TermOut{
							TermOut: &agentpb.TerminalOutput{
								SessionId: s.id,
								Data:      nil, // empty data signals EOF
							},
						},
					})
				}
				if s.recorder != nil {
					_ = s.recorder.Close()
				}
				close(s.stop)
				m.remove(s.id)
				return
			}
		}
	}()

	m.log.Info("terminal session opened", "session", req.GetSessionId(), "shell", req.GetShell(), "cols", cols, "rows", rows, "record", castPath)
	return nil
}

// Input writes user input to a session's PTY.
func (m *Manager) Input(req *agentpb.TerminalInput) error {
	s := m.get(req.GetSessionId())
	if s == nil {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}
	_, err := s.pty.Write(req.GetData())
	return err
}

// Resize changes a session's window size.
func (m *Manager) Resize(req *agentpb.TerminalResize) error {
	s := m.get(req.GetSessionId())
	if s == nil {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}
	return s.pty.Resize(uint16(req.GetCols()), uint16(req.GetRows()))
}

// Close terminates a session.
func (m *Manager) Close(req *agentpb.TerminalClose) error {
	s := m.get(req.GetSessionId())
	if s == nil {
		return nil
	}
	if s.recorder != nil {
		_ = s.recorder.Close()
	}
	err := s.pty.Close()
	m.remove(req.GetSessionId())
	m.log.Info("terminal session closed", "session", req.GetSessionId())
	return err
}

// CloseAll terminates every session (called on disconnect).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.recorder != nil {
			_ = s.recorder.Close()
		}
		_ = s.pty.Close()
		delete(m.sessions, id)
	}
}

func (m *Manager) get(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		_ = s.pty.Close()
		delete(m.sessions, id)
	}
}