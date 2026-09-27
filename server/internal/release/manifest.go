// Package release reads the release manifest shipped inside the dist
// tarball (deployed to /opt/watchman/manifest.json by install.sh).
//
// The manifest is the source of truth for agent upgrades: it carries the
// agent target version and the per-OS/arch SHA-256 of the agent binaries
// this control server actually serves from /opt/watchman/bin.
//
// Rationale: the control server's own build version (internal/version)
// freezes the moment the server binary is built. If the server is not
// rebuilt, "latest agent version" would freeze with it even when newer
// agent binaries are dropped into /opt/watchman/bin. The manifest
// decouples the two.
package release

import (
	"encoding/json"
	"fmt"
	"os"
)

// AgentEntry describes one agent binary in the release.
type AgentEntry struct {
	File   string `json:"file"`
	Sha256 string `json:"sha256"`
}

// Manifest is the release manifest (manifest.json).
type Manifest struct {
	Version   string                `json:"version"`
	Commit    string                `json:"commit"`
	BuildTime string                `json:"build_time"`
	Agents    map[string]AgentEntry `json:"agents"`
}

// Load reads and parses the manifest at path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if m.Version == "" {
		return nil, fmt.Errorf("manifest %s has empty version", path)
	}
	return &m, nil
}

// AgentSha256 returns the SHA-256 for the given GOOS/GOARCH pair
// (keyed as "linux/amd64"). ok is false when the manifest has no entry.
func (m *Manifest) AgentSha256(goos, goarch string) (sum string, ok bool) {
	if m == nil {
		return "", false
	}
	e, ok := m.Agents[goos+"/"+goarch]
	if !ok || e.Sha256 == "" {
		return "", false
	}
	return e.Sha256, true
}
