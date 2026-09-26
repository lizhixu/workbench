// Package config holds the agent's local configuration and persistent state.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the agent runtime config (from flags/env + file).
type Config struct {
	ServerAddr  string `json:"server_addr"`  // e.g. "localhost:9090"
	EnrollToken string `json:"enroll_token"` // one-time, only for first registration
	TLS         bool   `json:"tls"`          // use TLS to dial server
	// TLSServerName overrides the TLS server name check (for IP-based servers).
	TLSServerName string `json:"tls_server_name,omitempty"`
	// UpgradePubKey is the hex Ed25519 public key that must sign upgrades.
	UpgradePubKey string `json:"upgrade_pubkey,omitempty"`
	// StateFile is where the persistent agent identity (agent_id + auth_token)
	// is kept so the agent survives restarts.
	StateFile string `json:"-"`

	State State `json:"state"`
}

// State is the persistent agent identity.
type State struct {
	AgentID    string `json:"agent_id"`
	AuthToken  string `json:"auth_token"`
	Hostname   string `json:"hostname,omitempty"`
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

// Save persists the config (including state) to disk.
func (c *Config) Save() error {
	if c.StateFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.StateFile), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.StateFile, b, 0o600)
}