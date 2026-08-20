// Package ws implements the browser-facing WebSocket gateway for terminal
// sessions with multi-client pub/sub broadcasting (collaborative terminal sharing).
package ws

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

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

type sessionHub struct {
	mu      sync.Mutex
	sid     string
	agentID string
	clients map[*websocket.Conn]string // conn -> mode
}

var (
	hubMu       sync.Mutex
	activeHubs  = make(map[string]*sessionHub) // sid -> sessionHub
)

func getOrCreateHub(sid, agentID string) *sessionHub {
	hubMu.Lock()
	defer hubMu.Unlock()
	h, ok := activeHubs[sid]
	if !ok {
		h = &sessionHub{
			sid:     sid,
			agentID: agentID,
			clients: make(map[*websocket.Conn]string),
		}
		activeHubs[sid] = h
	}
	return h
}

func removeHubClient(sid string, conn *websocket.Conn) int {
	hubMu.Lock()
	defer hubMu.Unlock()
	h, ok := activeHubs[sid]
	if !ok {
		return 0
	}
	h.mu.Lock()
	delete(h.clients, conn)
	remaining := len(h.clients)
	h.mu.Unlock()
	if remaining == 0 {
		delete(activeHubs, sid)
	}
	return remaining
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

		sHub := getOrCreateHub(sid, agentID)
		sHub.mu.Lock()
		firstClient := len(sHub.clients) == 0
		sHub.clients[conn] = mode
		sHub.mu.Unlock()

		log.Info("terminal ws attached", "sid", sid, "agent", agentID, "mode", mode)

		// Send initial mode notice to client
		_ = conn.WriteJSON(Frame{Type: "notice", Sid: sid, Mode: mode})

		// If this is the first client attaching, register gRPC terminal output broadcaster and open PTY
		if firstClient {
			hub.SetTermHandler(sid, func(t *agentpb.TerminalOutput) {
				data := base64.StdEncoding.EncodeToString(t.GetData())
				frame := Frame{Type: "output", Sid: sid, Data: data}
				if len(t.GetData()) == 0 {
					frame = Frame{Type: "ended", Sid: sid}
				}

				sHub.mu.Lock()
				for c := range sHub.clients {
					_ = c.WriteJSON(frame)
				}
				sHub.mu.Unlock()
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
				_ = conn.WriteJSON(Frame{Type: "ended", Sid: sid, Reason: "agent gone"})
				return
			}
		}

		defer func() {
			rem := removeHubClient(sid, conn)
			if rem == 0 {
				hub.SetTermHandler(sid, nil)
				hub.Send(&agentpb.ServerMessage{
					Payload: &agentpb.ServerMessage_TermClose{TermClose: &agentpb.TerminalClose{SessionId: sid}},
				})
				if onEnd != nil {
					onEnd(sid)
				}
			}
		}()

		// Pump browser -> agent
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				log.Debug("ws read end", "sid", sid, "err", err)
				return
			}
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
				_ = conn.WriteJSON(Frame{Type: "pong"})
			}
		}
	}
}
