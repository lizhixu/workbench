// Package ws implements the browser-facing WebSocket gateway for terminal
// sessions with multi-client pub/sub broadcasting (collaborative terminal sharing).
package ws

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

// Frame is the JSON envelope exchanged with the browser over WebSocket.
type Frame struct {
	Type   string `json:"type"`             // input | resize | output | ended | ping | pong | notice
	Sid    string `json:"sid,omitempty"`    // session id
	Data   string `json:"data,omitempty"`   // base64-encoded bytes (input/output)
	Cols   uint32 `json:"cols,omitempty"`   // for resize
	Rows   uint32 `json:"rows,omitempty"`   // for resize
	Reason string `json:"reason,omitempty"` // for ended
	Mode   string `json:"mode,omitempty"`   // view | control | owner
}

// Timeouts for the browser-facing socket and for how long an unattended PTY is
// kept alive. A network blip must not destroy a working shell, so the PTY
// outlives the socket by sessionGrace (AGENTS.md A.3 "会话恢复").
const (
	// pongWait bounds how long we wait for any client frame. The browser pings
	// every 25s, so a silent socket past this is a hard disconnect (closed lid,
	// pulled cable) that TCP itself would not surface for another two hours.
	pongWait = 70 * time.Second
	// writeWait bounds a single write, so one stuck client cannot block the
	// broadcast loop for everyone sharing the session.
	writeWait = 10 * time.Second
	// sessionGrace is how long the PTY survives with no client attached.
	sessionGrace = 60 * time.Second
)

// sessionGraceForTest is the grace period actually used, so tests can shorten
// it instead of waiting a full minute.
var sessionGraceForTest = sessionGrace

type sessionHub struct {
	mu      sync.Mutex
	sid     string
	agentID string
	clients map[*websocket.Conn]string // conn -> mode

	// reapTimer is armed when the last client leaves and cancelled if someone
	// reattaches within sessionGrace. Non-nil means "pending teardown".
	reapTimer *time.Timer
}

var (
	hubMu      sync.Mutex
	activeHubs = make(map[string]*sessionHub) // sid -> sessionHub
)

// getOrCreateHub returns the hub for sid. The second result reports whether a
// PTY already exists for it: either clients are attached, or the hub is inside
// its grace period and its PTY is still alive on the agent. In both cases the
// caller must NOT reopen the PTY — it reattaches to the running shell.
func getOrCreateHub(sid, agentID string) (*sessionHub, bool) {
	hubMu.Lock()
	defer hubMu.Unlock()
	if h, ok := activeHubs[sid]; ok {
		h.mu.Lock()
		// Cancel a pending teardown: this client is the reconnect we waited for.
		if h.reapTimer != nil {
			h.reapTimer.Stop()
			h.reapTimer = nil
		}
		h.mu.Unlock()
		return h, true
	}
	h := &sessionHub{
		sid:     sid,
		agentID: agentID,
		clients: make(map[*websocket.Conn]string),
	}
	activeHubs[sid] = h
	return h, false
}

// detachClient removes conn from the session. When nobody is left the PTY is
// not killed right away: teardown is deferred by sessionGrace so a reconnecting
// browser finds its shell — with its scrollback and running processes — intact.
func detachClient(sid string, conn *websocket.Conn, teardown func()) {
	hubMu.Lock()
	h, ok := activeHubs[sid]
	if !ok {
		hubMu.Unlock()
		return
	}
	h.mu.Lock()
	delete(h.clients, conn)
	remaining := len(h.clients)
	if remaining == 0 && h.reapTimer == nil {
		h.reapTimer = time.AfterFunc(sessionGraceForTest, func() {
			hubMu.Lock()
			cur, still := activeHubs[sid]
			if !still || cur != h {
				hubMu.Unlock()
				return
			}
			cur.mu.Lock()
			empty := len(cur.clients) == 0
			cur.mu.Unlock()
			if !empty {
				// Someone reattached in the race window; keep the PTY.
				hubMu.Unlock()
				return
			}
			delete(activeHubs, sid)
			hubMu.Unlock()
			teardown()
		})
	}
	h.mu.Unlock()
	hubMu.Unlock()
}

// broadcast writes a frame to every attached client under a write deadline.
func (h *sessionHub) broadcast(f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		_ = c.SetWriteDeadline(time.Now().Add(writeWait))
		_ = c.WriteJSON(f)
	}
}

// TerminalHandler returns an http.HandlerFunc that upgrades to WebSocket and
// bridges the browser to the agent terminal session identified by :sid.
func TerminalHandler(reg *rpc.Registry, log *slog.Logger, onEnd func(sid string)) http.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("sid")
		shareToken := r.URL.Query().Get("share_token")
		shareCode := r.URL.Query().Get("code")

		mode := "owner" // default for authenticated owner

		// If connecting via share token or code
		if shareToken != "" || shareCode != "" {
			key := shareToken
			if key == "" {
				key = shareCode
			}
			share, ok := LookupShare(key)
			if !ok {
				http.Error(w, "invalid or expired share token", http.StatusUnauthorized)
				return
			}
			sid = share.SessionID
			mode = share.Mode
		}

		if sid == "" {
			http.Error(w, "missing sid", http.StatusBadRequest)
			return
		}

		sessInfo, ok := LookupSessionInfo(sid)
		if !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		agentID := sessInfo.AgentID
		hub := reg.Hub(agentID)
		if hub == nil {
			http.Error(w, "agent offline", http.StatusServiceUnavailable)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Warn("ws upgrade", "err", err)
			return
		}
		defer conn.Close()

		// A silent socket past pongWait is a hard disconnect. Every client frame
		// (input, resize, ping) and every pong pushes the deadline out.
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})

		sHub, ptyLive := getOrCreateHub(sid, agentID)
		sHub.mu.Lock()
		sHub.clients[conn] = mode
		sHub.mu.Unlock()

		log.Info("terminal ws attached", "sid", sid, "agent", agentID, "mode", mode, "reattach", ptyLive)

		// Send initial mode notice to client
		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		_ = conn.WriteJSON(Frame{Type: "notice", Sid: sid, Mode: mode})

		// Only open a PTY when there is not one already. A reattach (second
		// viewer, or the same browser coming back within the grace period) must
		// reuse the running shell, otherwise the user loses their scrollback and
		// any process still running in it.
		if !ptyLive {
			hub.SetTermHandler(sid, func(t *agentpb.TerminalOutput) {
				data := base64.StdEncoding.EncodeToString(t.GetData())
				frame := Frame{Type: "output", Sid: sid, Data: data}
				if len(t.GetData()) == 0 {
					frame = Frame{Type: "ended", Sid: sid}
				}
				sHub.broadcast(frame)
			})

			// Open PTY on agent
			shellCmd := sessInfo.Shell
			if !hub.Send(&agentpb.ServerMessage{
				Payload: &agentpb.ServerMessage_TermOpen{
					TermOpen: &agentpb.TerminalOpen{
						SessionId: sid,
						Shell:     shellCmd,
						Cols:      80,
						Rows:      24,
					},
				},
			}) {
				_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
				_ = conn.WriteJSON(Frame{Type: "ended", Sid: sid, Reason: "agent gone"})
				return
			}
		}

		defer detachClient(sid, conn, func() {
			// Nobody came back within the grace period: tear the PTY down and
			// drop the session mapping so sessionIdx does not grow forever.
			log.Info("terminal session reaped after grace period", "sid", sid, "grace", sessionGrace)
			hub.SetTermHandler(sid, nil)
			hub.Send(&agentpb.ServerMessage{
				Payload: &agentpb.ServerMessage_TermClose{TermClose: &agentpb.TerminalClose{SessionId: sid}},
			})
			UnregisterSession(sid)
			if onEnd != nil {
				onEnd(sid)
			}
		})

		// Pump browser -> agent
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				log.Debug("ws read end", "sid", sid, "err", err)
				return
			}
			// Any traffic proves the client is alive.
			_ = conn.SetReadDeadline(time.Now().Add(pongWait))
			var f Frame
			if err := json.Unmarshal(raw, &f); err != nil {
				continue
			}

			switch f.Type {
			case "input":
				// View mode clients cannot send keyboard inputs
				if mode == "view" {
					continue
				}
				data, err := base64.StdEncoding.DecodeString(f.Data)
				if err != nil {
					continue
				}
				hub.Send(&agentpb.ServerMessage{
					Payload: &agentpb.ServerMessage_TermIn{
						TermIn: &agentpb.TerminalInput{SessionId: sid, Data: data},
					},
				})
			case "resize":
				if mode == "view" {
					continue
				}
				hub.Send(&agentpb.ServerMessage{
					Payload: &agentpb.ServerMessage_TermResize{
						TermResize: &agentpb.TerminalResize{SessionId: sid, Cols: f.Cols, Rows: f.Rows},
					},
				})
			case "ping":
				_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
				_ = conn.WriteJSON(Frame{Type: "pong"})
			}
		}
	}
}
