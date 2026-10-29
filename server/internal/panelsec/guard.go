package panelsec

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/settings"
)

// Guard enforces panel domain restrictions and force-HTTPS policies.
type Guard struct {
	settings *settings.Store
	log      *slog.Logger

	mu       sync.Mutex
	lastWarn time.Time
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

// requestFromLoopback reports whether the TCP peer of the request is on this
// host. RemoteAddr is used instead of c.ClientIP(): gin trusts
// X-Forwarded-For by default, so the header would let a remote client claim
// 127.0.0.1 and slip past the port restriction. Same-host reverse proxies and
// tunnels (cloudflared, a local nginx) really do connect from 127.0.0.1 and
// must keep working.
func requestFromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	return isLoopback(host)
}

// localPort returns the TCP port the request was accepted on. net/http stores
// the listener address per request, which (unlike the Host header) cannot be
// forged by the client — a request to the panel's primary port is still
// recognised as such when the client rewrites Host. Zero means unavailable
// (tests, in-process handlers); callers must not restrict anything then.
func localPort(r *http.Request) int {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || addr == nil {
		return 0
	}
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

// portlessEntry returns the URL the panel should now be reached at without a
// port, or "" when no standard-port listener is currently serving. Selection
// mirrors HTTPSMiddleware: https when SSL is on and :443 serves, plain http
// when only :80 does.
func portlessEntry(sec settings.PanelSecurityConfig) string {
	domain := strings.TrimSpace(sec.PanelDomain)
	if domain == "" {
		return ""
	}
	if sec.SSLEnabled && StandardPortListening(443) {
		return "https://" + domain
	}
	if StandardPortListening(80) {
		return "http://" + domain
	}
	return ""
}

// primaryPortBlocked reports whether the request reached the panel's primary
// listener while the portless entry is serving. Binding a domain makes
// http(s)://<domain> on :80/:443 the only entry; the primary port is a
// documented fallback solely for when the standard ports cannot be bound
// (occupied, no privilege) — once they serve, the primary port must not act as
// a second entry (it would bypass the domain-only policy and keep serving TLS
// on an extra port).
func primaryPortBlocked(r *http.Request, sec settings.PanelSecurityConfig) (string, bool) {
	port := localPort(r)
	if port == 0 || port == 80 || port == 443 {
		return "", false
	}
	main, err := strconv.Atoi(PanelHTTPPort())
	if err != nil || main != port {
		return "", false
	}
	target := portlessEntry(sec)
	return target, target != ""
}

// DomainMiddleware checks the Host header when a panel domain is bound.
// Binding a domain automatically enables strict domain checking: the panel
// can then only be reached through that domain (loopback is always allowed
// for local maintenance; infrastructure paths such as agent enrollments are
// exempt). With no domain bound the panel stays reachable by IP.
// While the portless :80/:443 entry is serving, the primary port stops acting
// as a second entry for that domain: requests arriving on it are rejected and
// pointed at the portless URL. When no standard listener is serving the
// primary port remains the fallback.
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
		if !strings.EqualFold(host, wantDomain) {
			g.log.Warn("blocked direct IP or unauthorized domain access to panel",
				"requested_host", c.Request.Host,
				"allowed_domain", wantDomain,
				"client_ip", c.ClientIP(),
				"path", path,
			)

			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "直接 IP 访问或未授权域名已被面板安全策略拦截，请通过绑定的控制台域名访问",
			})
			return
		}

		// Same-host proxies/tunnels connect from loopback and legitimately
		// forward to the primary port; only remote clients are restricted.
		if requestFromLoopback(c.Request) {
			c.Next()
			return
		}
		if target, blocked := primaryPortBlocked(c.Request, sec); blocked {
			g.log.Warn("blocked panel access on the primary port while the portless entry is serving",
				"requested_host", c.Request.Host,
				"allowed_entry", target,
				"client_ip", c.ClientIP(),
				"method", c.Request.Method,
				"path", path,
			)
			// Browsers (and old bookmarks) are moved to the canonical entry;
			// other methods must not be silently replayed as GET by a redirect,
			// so API clients get an explicit error naming the URL to use.
			if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
				c.Redirect(http.StatusMovedPermanently, target+c.Request.RequestURI)
				c.Abort()
				return
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "面板已绑定域名，仅支持通过 " + target + " 访问；主端口仅在标准端口（:80/:443）不可用时作为回退",
			})
			return
		}

		c.Next()
	}
}

// redirectPort returns the port a forced HTTPS redirect would land on: the
// explicit port in the Host header, or 443 when the browser omitted it.
func redirectPort(r *http.Request) string {
	if _, port, err := net.SplitHostPort(r.Host); err == nil && port != "" {
		return port
	}
	return "443"
}

// httpsReachable reports whether the panel actually terminates TLS on the port
// a forced redirect would target. The portless :80 endpoint can only redirect
// to :443 when that listener is really serving; without it the redirect would
// land on a closed port.
func httpsReachable(r *http.Request) bool {
	switch port := redirectPort(r); port {
	case "80":
		// No TLS on the plaintext standard port.
		return false
	case "443":
		if main := PanelHTTPPort(); main == "443" {
			return true
		}
		return StandardPortListening(443)
	default:
		// An explicit non-standard port is the panel's primary listener (its
		// dynamic listener sniffs TLS in place), so HTTPS there is available.
		return true
	}
}

// warnThrottled logs a degraded-access warning at most once a minute so a
// browser stuck in a redirect loop cannot flood the log.
func (g *Guard) warnThrottled(msg string, args ...any) {
	g.mu.Lock()
	now := time.Now()
	emit := now.Sub(g.lastWarn) > time.Minute
	if emit {
		g.lastWarn = now
	}
	g.mu.Unlock()
	if emit {
		g.log.Warn(msg, args...)
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
			// Redirecting is only safe when the target really serves TLS. When
			// the portless :443 listener failed to bind (the port is held by
			// another service, or the deployment does not publish it), a blind
			// 301 sends every browser to a dead endpoint — the panel then looks
			// unreachable on both 80 and 443 even though :80 is fine. Keep
			// serving over HTTP in that degraded state and let the settings UI
			// report the listener error.
			if !httpsReachable(c.Request) {
				g.warnThrottled("panel force-HTTPS redirect skipped: standard :443 listener is not serving; panel stays reachable over HTTP",
					"host", c.Request.Host, "path", path)
				c.Next()
				return
			}
			target := "https://" + c.Request.Host + c.Request.RequestURI
			c.Redirect(http.StatusMovedPermanently, target)
			c.Abort()
			return
		}

		c.Next()
	}
}
