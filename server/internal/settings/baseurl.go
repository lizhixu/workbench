package settings

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// This file is the single resolution path for the panel's externally
// reachable base URL. Every URL the panel hands to an outside party — agent
// install commands, agent binary downloads, GitHub App webhooks, per-app Git
// webhooks, volume-restore callbacks, the operator tunnel endpoint — must go
// through BaseURL/RequestBaseURL. Per-feature fallback chains used to drift
// apart (some read a setting, some only a startup flag, some trusted the
// request Host blindly); keeping one chain is what makes the panel's public
// address consistent across all of them.
//
// The bound panel domain (security.panel_domain) is the only configured
// address source. The retired server.public_url setting is migrated into it
// once at load (see migrateRetiredPublicURL), and the -public-url startup flag
// is accepted but ignored so old service units still boot.
//
// Precedence, highest first:
//
//  1. security.panel_domain — the bound panel domain, with https when panel
//     SSL or force-HTTPS is on, or when the inbound request itself is https
//     (TLS or X-Forwarded-Proto: a proxy that terminates TLS must not force
//     the admin to flip 「强制 HTTPS」 just to advertise the right scheme).
//     The domain is served on the standard ports, so no port is appended.
//  2. the inbound request's scheme://Host — an unbound panel is reached by
//     IP, so that address is what external parties get.
//
// Case 1 counts as configured; case 2 is a guess derived from how the browser
// happened to reach the panel and callers may refuse it (the volume restore
// callback does, because a client-controlled Host header must not decide
// where an agent downloads a backup).

// BaseURL resolves the panel base URL from the settings snapshot and the
// request's scheme/host. configured is false only when no domain is bound and
// the result came from the request itself.
func (p PanelSecurityConfig) BaseURL(reqScheme, reqHost string) (string, bool) {
	if domain := strings.TrimSpace(p.PanelDomain); domain != "" {
		scheme := "http"
		if p.SSLEnabled || p.ForceHTTPS || strings.EqualFold(reqScheme, "https") {
			scheme = "https"
		}
		return scheme + "://" + domain, true
	}
	host := strings.TrimSpace(reqHost)
	if host == "" {
		return "", false
	}
	if !strings.EqualFold(reqScheme, "https") {
		reqScheme = "http"
	}
	return reqScheme + "://" + host, false
}

// BaseURL resolves the panel base URL using the store's current settings.
// A nil store (tests) behaves like an unconfigured panel.
func (s *Store) BaseURL(reqScheme, reqHost string) (string, bool) {
	if s == nil {
		return PanelSecurityConfig{}.BaseURL(reqScheme, reqHost)
	}
	return s.PanelSecurity().BaseURL(reqScheme, reqHost)
}

// RequestBaseURL resolves the panel base URL for an inbound request. The
// scheme comes from TLS or X-Forwarded-Proto; a store may be nil in tests.
func RequestBaseURL(c *gin.Context, store *Store) (string, bool) {
	scheme := "http"
	if c != nil && c.Request != nil {
		if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
	}
	host := ""
	if c != nil && c.Request != nil {
		host = c.Request.Host
	}
	return store.BaseURL(scheme, host)
}
