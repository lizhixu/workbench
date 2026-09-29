package panelsec

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/settings"
)

// Guard enforces panel domain restrictions and force-HTTPS policies.
type Guard struct {
	settings *settings.Store
	log      *slog.Logger
}

// NewGuard builds a panel security guard.
func NewGuard(st *settings.Store, log *slog.Logger) *Guard {
	if log == nil {
		log = slog.Default()
	}
	return &Guard{settings: st, log: log}
}

// exemptPath returns true if the path is an essential infrastructure endpoint
// that must never be blocked by strict domain policies (e.g. agent heartbeats,
// agent installs, enrollments, webhooks).
func exemptPath(p string) bool {
	if strings.HasPrefix(p, "/api/v1/secure-entry/") {
		return true
	}
	switch p {
	case "/api/v1/hosts/enroll",
		"/api/v1/host/install_script",
		"/api/v1/install",
		"/api/v1/agent/binary",
		"/install",
		"/install_script",
		"/agent/binary":
		return true
	}
	for _, pre := range []string{
		"/api/v1/apps/webhook/",
		"/api/v1/terminals/share/",
		"/agent/",
	} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// isLoopback reports whether host is a loopback address or localhost.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DomainMiddleware checks the Host header when a panel domain is bound.
// Binding a domain automatically enables strict domain checking: the panel
// can then only be reached through that domain (loopback is always allowed
// for local maintenance; infrastructure paths such as agent enrollments are
// exempt). With no domain bound the panel stays reachable by IP.
func (g *Guard) DomainMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if g.settings == nil {
			c.Next()
			return
		}
		sec := g.settings.PanelSecurity()
		if sec.PanelDomain == "" {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if exemptPath(path) {
			c.Next()
			return
		}

		host := c.Request.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		// Bracketed IPv6 literals without a port (e.g. "[::1]") fail
		// SplitHostPort above; strip the brackets so net.ParseIP works.
		host = strings.Trim(strings.TrimSpace(host), "[]")
		host = strings.ToLower(host)

		// Loopback access (127.0.0.1 / localhost) is always allowed for local maintenance.
		if isLoopback(host) {
			c.Next()
			return
		}

		wantDomain := strings.TrimSpace(strings.ToLower(sec.PanelDomain))
		if strings.EqualFold(host, wantDomain) {
			c.Next()
			return
		}

		g.log.Warn("blocked direct IP or unauthorized domain access to panel",
			"requested_host", c.Request.Host,
			"allowed_domain", wantDomain,
			"client_ip", c.ClientIP(),
			"path", path,
		)

		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "直接 IP 访问或未授权域名已被面板安全策略拦截，请通过绑定的控制台域名访问",
		})
	}
}

// HTTPSMiddleware redirects plaintext HTTP requests to HTTPS when panel SSL
// and force HTTPS are enabled.
func (g *Guard) HTTPSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if g.settings == nil {
			c.Next()
			return
		}
		sec := g.settings.PanelSecurity()
		if !sec.SSLEnabled || !sec.ForceHTTPS {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if exemptPath(path) {
			c.Next()
			return
		}

		isTLS := c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https"
		if !isTLS {
			target := "https://" + c.Request.Host + c.Request.RequestURI
			c.Redirect(http.StatusMovedPermanently, target)
			c.Abort()
			return
		}

		c.Next()
	}
}
