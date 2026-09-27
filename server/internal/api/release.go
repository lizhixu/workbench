package api

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"watchman/server/internal/release"
)

// agentRelease is the release manifest loaded at startup (nil when no
// manifest is deployed, e.g. dev builds). When present it overrides
// CurrentAgentVersion as the source of truth for agent upgrades: the
// control server's own build version freezes at build time, while the
// manifest tracks the agent binaries actually shipped in /opt/watchman.
var agentRelease *release.Manifest

// publicURL overrides the request Host header when building absolute
// URLs handed to agents (binary downloads). Needed behind reverse
// proxies / NAT / port mapping where the Host header does not point
// at the agent-facing address.
var publicURL string

// SetAgentRelease installs the release manifest (nil clears it).
func SetAgentRelease(m *release.Manifest) { agentRelease = m }

// SetPublicURL sets the agent-facing public base URL
// (e.g. https://watchman.example.com).
func SetPublicURL(u string) { publicURL = strings.TrimSuffix(strings.TrimSpace(u), "/") }

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

// upgradeDownloadBase returns the absolute base URL for agent-facing
// downloads: -public-url when configured, else scheme://Host.
func upgradeDownloadBase(c *gin.Context) string {
	if publicURL != "" {
		return publicURL
	}
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, c.Request.Host)
}
