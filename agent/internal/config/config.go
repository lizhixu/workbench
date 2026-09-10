// Package config holds the agent's local configuration and persistent state.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
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
