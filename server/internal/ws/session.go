package ws

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// upgrader upgrades HTTP to WebSocket.
var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  8192,
	WriteBufferSize: 8192,
}

// SessionInfo holds metadata for a registered terminal session.
type SessionInfo struct {
	AgentID   string
	Shell     string
	CreatedAt time.Time
}

// ShareInfo holds metadata for a terminal sharing token.
type ShareInfo struct {
	Token     string
	Code      string // 6-character user-friendly code, e.g. "2gw937"
	SessionID string
	AgentID   string
	Mode      string // "view" | "control"
	ExpiresAt time.Time
}

var (
	sessionMu  sync.RWMutex
	sessionIdx = make(map[string]SessionInfo) // sid -> SessionInfo

	shareMu  sync.RWMutex
	shareIdx = make(map[string]*ShareInfo) // token/code -> ShareInfo
)

// RegisterSession records that session sid belongs to agentID with an optional custom shell.
func RegisterSession(sid, agentID string, shell ...string) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	sh := ""
	if len(shell) > 0 {
		sh = shell[0]
	}
	sessionIdx[sid] = SessionInfo{
		AgentID:   agentID,
		Shell:     sh,
		CreatedAt: time.Now(),
	}
}

// LookupSession returns the agent_id owning a session, or "".
func LookupSession(sid string) string {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return sessionIdx[sid].AgentID
}

// LookupSessionInfo returns full session info.
func LookupSessionInfo(sid string) (SessionInfo, bool) {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	info, ok := sessionIdx[sid]
	return info, ok
}

// UnregisterSession removes a session mapping.
func UnregisterSession(sid string) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	delete(sessionIdx, sid)
}

// CreateShare generates a share token and friendly code for a terminal session.
func CreateShare(sid, agentID, mode string, duration time.Duration) (*ShareInfo, error) {
	if mode != "control" {
		mode = "view"
	}
	if duration <= 0 {
		duration = 15 * time.Minute
	}

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)

	code := generateCode(6)
	expiresAt := time.Now().Add(duration)

	info := &ShareInfo{
		Token:     token,
		Code:      code,
		SessionID: sid,
		AgentID:   agentID,
		Mode:      mode,
		ExpiresAt: expiresAt,
	}

	shareMu.Lock()
	shareIdx[token] = info
	shareIdx[code] = info
	shareMu.Unlock()

	return info, nil
}

// LookupShare validates and returns share information by token or code.
func LookupShare(tokenOrCode string) (*ShareInfo, bool) {
	shareMu.RLock()
	defer shareMu.RUnlock()
	info, ok := shareIdx[tokenOrCode]
	if !ok {
		return nil, false
	}
	if time.Now().After(info.ExpiresAt) {
		return nil, false
	}
	return info, true
}

func generateCode(n int) string {
	const charset = "23456789abcdefghjkmnpqrstuvwxyz"
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		num, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		out[i] = charset[num.Int64()]
	}
	return string(out)
}
