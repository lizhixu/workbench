// Persisted CLI configuration: ~/.watchmanctl.json (mode 0600), written by
// `watchmanctl login` and consumed by every other subcommand.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// config is the on-disk shape of ~/.watchmanctl.json.
type config struct {
	// Server is the panel base URL, e.g. https://panel.example.com:18789.
	Server string `json:"server"`
	// Token is the JWT issued by POST /api/v1/auth/login.
	Token string `json:"token"`
	// Insecure skips TLS verification on both the REST control plane and the
	// gRPC data plane (self-signed panels). Persisted so later runs behave
	// consistently; every run prints a warning when it is active.
	Insecure bool `json:"insecure"`
}

// defaultConfigPath resolves ~/.watchmanctl.json.
func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".watchmanctl.json"), nil
}

// loadConfig reads the config file; a missing file yields an empty config
// (every subcommand then complains about login, which is the wanted UX).
func loadConfig() (*config, error) {
	p, err := defaultConfigPath()
	if err != nil {
		return nil, err
	}
	return loadConfigFrom(p)
}

func loadConfigFrom(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// save persists the config via the default path.
func (c *config) save() error {
	p, err := defaultConfigPath()
	if err != nil {
		return err
	}
	return c.saveTo(p)
}

func (c *config) saveTo(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// The file carries a session JWT: guard it like a private key.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	// Explicit chmod: WriteFile's perm is ignored when the file already exists
	// (e.g. left world-readable by an older build) — enforce 0600 every save.
	return os.Chmod(path, 0o600)
}
