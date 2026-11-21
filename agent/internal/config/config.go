// Package config holds the agent's local configuration and persistent state.
package config

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Redirect timing: how long the agent probes a redirect target before
// deciding it is unreachable, how long a redirected address must keep
// failing before the agent falls back to the previous server, and how
// long a failed target is ignored before the agent will try it again.
// The fallback only fires when the previous server still answers a TCP
// probe — if the old control plane is already gone, the agent keeps
// dialing the new address instead of flapping back to a dead one.
const (
	RedirectProbeTimeout  = 3 * time.Second
	RedirectFallbackAfter = 10 * time.Minute
	RedirectCooldown      = 15 * time.Minute
	// RedirectLoopGuard is how long a reversal is refused after a
	// redirect was applied: if two control planes redirect at each
	// other (stale setting on both sides, or a rollback issued while the
	// old server still points at the new one), the agent would otherwise
	// flap between them every second. Refusing the reversal for a few
	// minutes turns that into one flip per window — the host stays
	// usable and the panel shows the flip-flopping — while a deliberate
	// rollback still lands once the window passes.
	RedirectLoopGuard = 5 * time.Minute
)

// Config is the agent runtime config (from flags/env + file).
//
// State is reached from several goroutines: the dialer persists the identity
// after registering, the receive loop records a server maintenance notice, and
// the upgrade callback records the "upgrade" reconnect reason before exiting.
// Mutate or read it only through Update / Snapshot / Save.
type Config struct {
	mu sync.Mutex

	ServerAddr  string `json:"server_addr"`  // e.g. "localhost:9090"
	EnrollToken string `json:"enroll_token"` // one-time, only for first registration
	TLS         bool   `json:"tls"`          // use TLS to dial server
	// TLSServerName overrides the TLS server name check (for IP-based servers).
	TLSServerName string `json:"tls_server_name,omitempty"`
	// UpgradePubKey is the hex Ed25519 public key that must sign upgrades.
	UpgradePubKey string `json:"upgrade_pubkey,omitempty"`
	// RedirectTarget is the control-plane address the server told this
	// agent to dial (host:port, "" = no redirect). It outranks ServerAddr:
	// the -server flag is the operator-configured default, a live redirect
	// is the newer instruction. An empty target pushed by the server means
	// "no opinion" and never clears it. It is cleared by falling back
	// after RedirectFallbackAfter of failures, or by the operator
	// changing the -server flag (see main.go).
	RedirectTarget string `json:"redirect_target,omitempty"`
	// RedirectPrevious is the address in use before RedirectTarget was
	// applied; the fallback target when the redirect proves unreachable.
	RedirectPrevious string `json:"redirect_previous,omitempty"`
	// RedirectAppliedAt is the unix time RedirectTarget was applied.
	RedirectAppliedAt int64 `json:"redirect_applied_at,omitempty"`
	// FailedRedirectTarget / FailedRedirectAt record the last redirect
	// target that was applied and then failed for RedirectFallbackAfter
	// (the agent fell back). The same target is ignored until the
	// cooldown expires, so a stale redirect cannot flap the agent.
	FailedRedirectTarget string `json:"failed_redirect_target,omitempty"`
	FailedRedirectAt     int64  `json:"failed_redirect_at,omitempty"`
	// StateFile is where the persistent agent identity (agent_id + auth_token)
	// is kept so the agent survives restarts.
	StateFile string `json:"-"`

	State State `json:"state"`
}

// State is the persistent agent identity.
type State struct {
	AgentID         string `json:"agent_id"`
	AuthToken       string `json:"auth_token"`
	Hostname        string `json:"hostname,omitempty"`
	ReconnectReason string `json:"reconnect_reason,omitempty"` // "upgrade", "maintenance"
}

// DefaultStatePath returns a sensible default location for the agent state file.
func DefaultStatePath() string {
	// Windows: %ProgramData%\watchman-agent\state.json
	// Others:  /var/lib/watchman-agent/state.json  (or cwd if undeterminable)
	if dir := os.Getenv("ProgramData"); dir != "" {
		return filepath.Join(dir, "watchman-agent", "state.json")
	}
	for _, candidate := range []string{"/var/lib/watchman-agent", "."} {
		if abs, err := filepath.Abs(candidate); err == nil {
			return filepath.Join(abs, "state.json")
		}
	}
	return "state.json"
}

// Load reads config from the given path, or returns an empty config if it
// doesn't exist yet.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{StateFile: path}, nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	c.StateFile = path
	return &c, nil
}

// Update applies a mutation to the persistent state and saves it atomically.
// It is the only safe way to write State from a goroutine.
func (c *Config) Update(fn func(*State)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn != nil {
		fn(&c.State)
	}
	return c.saveLocked()
}

// Snapshot returns a copy of the persistent state.
func (c *Config) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.State
}

// ReconnectReason returns the persisted reconnect reason (see AGENTS.md 8.6).
func (c *Config) ReconnectReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.State.ReconnectReason
}

// SetReconnectReason persists a reconnect reason and saves it atomically.
func (c *Config) SetReconnectReason(reason string) error {
	return c.Update(func(s *State) { s.ReconnectReason = reason })
}

// NormalizeServerAddr strips any URL scheme/path off a control-plane
// address and fills in the default :9090 port, so "wx1.us.ci",
// "wx1.us.ci:9090" and "https://wx1.us.ci/" all dial the same place.
func NormalizeServerAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	if idx := strings.Index(addr, "/"); idx != -1 {
		addr = addr[:idx]
	}
	if addr != "" && !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, "9090")
	}
	return addr
}

// RedirectState returns the active redirect target and previous address
// (both "" when no redirect is active).
func (c *Config) RedirectState() (target, previous string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RedirectTarget, c.RedirectPrevious
}

// EffectiveAddr is the address the agent actually dials: a live redirect
// outranks the -server flag.
func (c *Config) EffectiveAddr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.RedirectTarget != "" {
		return NormalizeServerAddr(c.RedirectTarget)
	}
	return NormalizeServerAddr(c.ServerAddr)
}

// RedirectReversal reports whether target is the address the agent
// dialed before its most recent redirect and that redirect was applied
// within RedirectLoopGuard. The two control planes are likely
// redirecting at each other (stale setting on both sides, or a rollback
// issued while the old server still points at the new one); flipping
// back immediately would flap the agent between them every second.
func (c *Config) RedirectReversal(target string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.RedirectTarget == "" || c.RedirectPrevious == "" {
		return false
	}
	if NormalizeServerAddr(target) != NormalizeServerAddr(c.RedirectPrevious) {
		return false
	}
	return now.Unix()-c.RedirectAppliedAt < int64(RedirectLoopGuard/time.Second)
}

// RedirectCoolingDown reports whether target failed recently enough that
// the agent must ignore another push of the same target.
func (c *Config) RedirectCoolingDown(target string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	target = NormalizeServerAddr(target)
	if target == "" || c.FailedRedirectTarget == "" {
		return false
	}
	if NormalizeServerAddr(c.FailedRedirectTarget) != target {
		return false
	}
	return now.Unix()-c.FailedRedirectAt < int64(RedirectCooldown/time.Second)
}

// SetRedirect records a server-pushed redirect and persists it, so the
// new address survives an agent restart. previous is the address in use
// before the switch (the fallback target).
func (c *Config) SetRedirect(target, previous string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.RedirectTarget = NormalizeServerAddr(target)
	c.RedirectPrevious = NormalizeServerAddr(previous)
	c.RedirectAppliedAt = now.Unix()
	if NormalizeServerAddr(c.FailedRedirectTarget) == c.RedirectTarget {
		c.FailedRedirectTarget = ""
		c.FailedRedirectAt = 0
	}
	return c.saveLocked()
}

// ClearRedirect drops any active redirect (and any failure record): the
// agent goes back to dialing its -server flag address.
func (c *Config) ClearRedirect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.RedirectTarget = ""
	c.RedirectPrevious = ""
	c.RedirectAppliedAt = 0
	c.FailedRedirectTarget = ""
	c.FailedRedirectAt = 0
	return c.saveLocked()
}

// NoteRedirectFailed records that the active redirect target proved
// unreachable and the agent fell back: the target is remembered with a
// timestamp so it is not re-applied until the cooldown expires, and the
// redirect itself is cleared.
func (c *Config) NoteRedirectFailed(now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.RedirectTarget != "" {
		c.FailedRedirectTarget = c.RedirectTarget
		c.FailedRedirectAt = now.Unix()
	}
	c.RedirectTarget = ""
	c.RedirectPrevious = ""
	c.RedirectAppliedAt = 0
	return c.saveLocked()
}

// FailedTargetForReport returns the failed redirect target while its
// cooldown is still active, "" once it expired (or when there is none).
// The agent reports this in RegisterRequest so the panel can show a
// failed move; a stale failure clears itself on the next registration
// after the cooldown.
func (c *Config) FailedTargetForReport(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.FailedRedirectTarget == "" {
		return ""
	}
	if now.Unix()-c.FailedRedirectAt >= int64(RedirectCooldown/time.Second) {
		return ""
	}
	return NormalizeServerAddr(c.FailedRedirectTarget)
}

// SyncAgentConf rewrites the server= line of the install-time agent.conf
// next to the state file so the on-disk record matches the address the
// agent actually dials. Best-effort: the agent never reads agent.conf
// (the -server flag and the redirect state in state.json are
// authoritative), so a missing file or a write error is a no-op, not a
// redirect failure.
func (c *Config) SyncAgentConf(addr string) error {
	if c.StateFile == "" {
		return nil
	}
	path := filepath.Join(filepath.Dir(c.StateFile), "agent.conf")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil // no agent.conf (e.g. non-Linux install): nothing to sync
	}
	addr = NormalizeServerAddr(addr)
	lines := strings.Split(string(b), "\n")
	found := false
	for i, ln := range lines {
		if strings.HasPrefix(ln, "server=") {
			lines[i] = "server=" + addr
			found = true
		}
	}
	if !found {
		// Drop the empty element left by a trailing newline so the new
		// line lands right after the last real line, then keep the file
		// newline-terminated.
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1]
		}
		lines = append(lines, "server="+addr, "")
	}
	out := strings.Join(lines, "\n")
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-conf-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, info.Mode()); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Save persists the config (including state) to disk.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

// saveLocked writes the state file via a temporary file + rename so a crash
// (notably the os.Exit right after an agent self-upgrade) can never truncate
// the file and lose the persisted identity.
// Callers must hold c.mu.
func (c *Config) saveLocked() error {
	if c.StateFile == "" {
		return nil
	}
	dir := filepath.Dir(c.StateFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, c.StateFile)
}
