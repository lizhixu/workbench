package api

import (
	"watchman/server/internal/release"
)

// agentRelease is the release manifest loaded at startup (nil when no
// manifest is deployed, e.g. dev builds). When present it overrides
// CurrentAgentVersion as the source of truth for agent upgrades: the
// control server's own build version freezes at build time, while the
// manifest tracks the agent binaries actually shipped in /opt/watchman.
var agentRelease *release.Manifest

// manifestPath stores the configured release manifest file path.
var manifestPath string

// SetManifestPath records the path to manifest.json (e.g. /opt/watchman/manifest.json).
func SetManifestPath(p string) { manifestPath = p }

// ManifestPath returns the path to manifest.json.
func ManifestPath() string { return manifestPath }

// SetAgentRelease installs the release manifest (nil clears it).
func SetAgentRelease(m *release.Manifest) { agentRelease = m }

// agentTargetVersion is the version agents should run.
func agentTargetVersion() string {
	if agentRelease != nil {
		return agentRelease.Version
	}
	return CurrentAgentVersion
}

// manifestAgentSha256 returns the manifest's SHA-256 for os/arch.
// ok is false when no manifest is loaded or it has no entry.
func manifestAgentSha256(goos, goarch string) (sum string, ok bool) {
	if agentRelease == nil {
		return "", false
	}
	return agentRelease.AgentSha256(goos, goarch)
}
