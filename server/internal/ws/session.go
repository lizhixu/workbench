package ws

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// upgrader upgrades HTTP to WebSocket. Origin is verified because a WebSocket
// upgrade is not subject to the same-origin policy: without this check any page
// the operator visits could open a terminal or file channel using the browser's
// stored token.
var upgrader = websocket.Upgrader{
	CheckOrigin:     checkOrigin,
	ReadBufferSize:  8192,
	WriteBufferSize: 8192,
}

var (
	originMu       sync.RWMutex
	allowedOrigins []string
)

// SetAllowedOrigins configures browser origins permitted to open WebSocket
// connections in addition to the same-origin default. An entry may be a host,
// a host:port, or a full origin URL; the single entry "*" disables the check
// and must only be used when no untrusted browser can reach the server.
func SetAllowedOrigins(list []string) {
	cleaned := make([]string, 0, len(list))
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if u, err := url.Parse(item); err == nil && u.Host != "" {
			item = u.Host
		}
		cleaned = append(cleaned, item)
	}
	originMu.Lock()
	allowedOrigins = cleaned
	originMu.Unlock()
}

// checkOrigin reports whether the upgrade request's Origin may open a session.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Only browsers send Origin, and they always send it on a WebSocket
		// upgrade. An absent header therefore means a non-browser client (CLI,
		// tests, agent tooling), which cannot be a cross-site hijack.
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		slog.Warn("ws origin rejected (unparsable)", "origin", origin)
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}

	originMu.RLock()
	list := allowedOrigins
	originMu.RUnlock()
	for _, a := range list {
		if a == "*" || strings.EqualFold(a, u.Host) {
			return true
		}
	}

	// The Vite dev server proxies /api to the control server, so in development
	// the page origin (localhost:5173) differs from the request host
	// (localhost:18080). Accept that only when both sides are loopback, which a
	// page served from a remote site can never satisfy.
	if isLoopbackHost(u.Host) && isLoopbackHost(r.Host) {
		return true
	}

	slog.Warn("ws origin rejected", "origin", origin, "host", r.Host)
	return false
}

// isLoopbackHost reports whether a host or host:port refers to the loopback
// interface.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
