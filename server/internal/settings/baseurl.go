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
//  1. security.panel_domain — the bound panel domain. Its scheme and port
//     come from what the panel actually serves, never from how this one
//     request happened to arrive:
//     - https://<domain> when panel SSL is on and :443 is actually
//       listening (standard-port listener or the primary listener on :443);
//     - http://<domain> when :80 is actually listening;
//     - <scheme>://<domain>:<primary port> when neither standard port is
//       serving — the primary port is the documented fallback entry then,
//       and its scheme follows panel SSL (the dynamic listener sniffs
//       TLS on it when SSL is on, plaintext otherwise);
//     - without serving-state information (tests, embedded use) the
//       setting alone decides: https when panel SSL is on, else http,
//       on the standard ports.
//     The request's scheme must NOT decide this: a bound-domain URL
//     carries no port, so it always means the standard ports, and a
//     request that arrived over https on the primary port proves nothing
//     about :443. ForceHTTPS is a redirect policy, not a serving
//     capability, so it does not decide the scheme either. The panel is
//     the outermost service by design, so no reverse proxy in front of
//     it is assumed: X-Forwarded-Proto is never trusted anywhere in this
//     file, and a proxy that terminates TLS is not inferred from it — to
//     advertise https, enable panel SSL so the panel itself serves :443.
//  2. the inbound request's scheme://Host — an unbound panel is reached by
//     IP, so that address is what external parties get. This is the only
//     case where the request scheme is used, and it means direct TLS
//     only (c.Request.TLS != nil), never a forwarded header, because the
//     address it produces is exactly the address the request arrived at, port
//     included.
//
// Case 1 counts as configured; case 2 is a guess derived from how the browser
// happened to reach the panel and callers may refuse it (the volume restore
// callback does, because a client-controlled Host header must not decide
// where an agent downloads a backup).

// ServingState describes what the panel actually serves right now. It is
// supplied by the panel security layer (which owns the listeners) through
// SetServingStateProvider; settings cannot import that layer (import cycle),
// so the state arrives as a callback instead of a direct call.
type ServingState struct {
	// HTTPSListening reports whether :443 is currently serving the panel
	// (standard-port listener, or the primary listener when it is :443).
	HTTPSListening bool
	// HTTPListening reports whether :80 is currently serving the panel.
	HTTPListening bool
	// PrimaryPort is the panel's primary listen port ("" when unknown).
	// It is the fallback entry when neither standard port serves.
	PrimaryPort string
}

// BaseURL resolves the panel base URL from the settings snapshot and the
// request's scheme/host. configured is false only when no domain is bound and
// the result came from the request itself.
//
// For a bound domain the request scheme is deliberately ignored: see the
// file header. Only the unbound fallback uses it.
func (p PanelSecurityConfig) BaseURL(reqScheme, reqHost string) (string, bool) {
	if domain := strings.TrimSpace(p.PanelDomain); domain != "" {
		scheme := "http"
		if p.SSLEnabled {
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
// A nil store (tests) behaves like an unconfigured panel. When a serving
// state provider is registered, a bound domain follows the actual listener
// state instead of the setting alone; without one it falls back to the
// setting-only rule in PanelSecurityConfig.BaseURL.
func (s *Store) BaseURL(reqScheme, reqHost string) (string, bool) {
	if s == nil {
		return PanelSecurityConfig{}.BaseURL(reqScheme, reqHost)
	}
	sec := s.PanelSecurity()
	domain := strings.TrimSpace(sec.PanelDomain)
	if domain == "" {
		return sec.BaseURL(reqScheme, reqHost)
	}
	if provider := s.servingStateProvider(); provider != nil {
		st := provider()
		if sec.SSLEnabled && st.HTTPSListening {
			return "https://" + domain, true
		}
		if st.HTTPListening {
			return "http://" + domain, true
		}
		if st.PrimaryPort != "" {
			scheme := "http"
			if sec.SSLEnabled {
				scheme = "https"
			}
			return scheme + "://" + domain + ":" + st.PrimaryPort, true
		}
	}
	return sec.BaseURL(reqScheme, reqHost)
}

// RequestBaseURL resolves the panel base URL for an inbound request. The
// request scheme is only used for the unbound fallback, and it means
// direct TLS only (c.Request.TLS != nil): the panel is the outermost
// service, so X-Forwarded-Proto is never trusted. A bound domain ignores
// the request scheme entirely (see the file header). A store may be nil
// in tests.
func RequestBaseURL(c *gin.Context, store *Store) (string, bool) {
	scheme := "http"
	if c != nil && c.Request != nil {
		if c.Request.TLS != nil {
			scheme = "https"
		}
	}
	host := ""
	if c != nil && c.Request != nil {
		host = c.Request.Host
	}
	return store.BaseURL(scheme, host)
}
