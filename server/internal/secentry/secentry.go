// Package secentry implements the control-plane "secure entry" (安全入口):
// once enabled, the panel can only be logged in through a secret entry URL.
// Direct visits to the normal pages (/login, /) and unauthenticated API
// calls without a valid entry cookie are answered with a neutral 404, so a
// scanner cannot even tell a Watchman panel is behind the address.
//
// Flow:
//  1. Admin sets security.secure_entry_enabled=true and
//     security.secure_entry_path=<secret> in system settings (admin-only,
//     audited; see server/internal/settings).
//  2. Visiting GET /api/v1/secure-entry/<secret> sets an HttpOnly cookie
//     wm_secure_entry=<secret>.<HMAC(secret)> and redirects to /.
//     The HMAC key is derived from the server's JWT signing key with domain
//     separation, so cookies cannot be forged without server access.
//  3. Guard middleware: when enabled, every request must either carry a
//     valid JWT (already logged in), a valid entry cookie, or target an
//     explicitly exempt infrastructure path (agent install/enroll,
//     webhooks, the entry endpoints themselves). Everything else gets 404.
//     The web console's static pages are served by the server itself
//     (server/web, go:embed) and are hidden by the same guard — no reverse
//     proxy is involved.
//
// Recovery: if the entry path is lost, run
// `watchman-server -reset-secure-entry` on the control host, or edit
// <dataDir>/settings.json and flip security.secure_entry_enabled to false.
package secentry

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"watchman/server/internal/auth"
	"watchman/server/internal/settings"
)

// CookieName is the entry-proof cookie set after visiting the secret entry.
const CookieName = "wm_secure_entry"

// cookieMaxAge is 7 days: long enough to be convenient, short enough that a
// leaked cookie does not grant indefinite login attempts (it never grants a
// session by itself — it only unlocks the login endpoint).
const cookieMaxAge = 7 * 24 * 3600

// hmacDomain separates the entry-cookie key from the JWT signing key.
const hmacDomain = "watchman-secure-entry-v1"

// DeriveKey derives the entry-cookie HMAC key from the JWT signing key.
// Domain separation keeps the two usages cryptographically independent.
func DeriveKey(jwtKey []byte) []byte {
	m := hmac.New(sha256.New, jwtKey)
	m.Write([]byte(hmacDomain))
	return m.Sum(nil)
}

// Guard enforces the secure entry on every HTTP request.
type Guard struct {
	settings *settings.Store
	auth     *auth.Store
	key      []byte
	log      *slog.Logger
}

// NewGuard builds the guard. A nil settings store disables the feature
// (fail-open only when the store is absent entirely, e.g. in unit tests);
// a nil auth store means "no JWT fast-path".
func NewGuard(settingsStore *settings.Store, authStore *auth.Store, jwtKey []byte, log *slog.Logger) *Guard {
	if log == nil {
		log = slog.Default()
	}
	return &Guard{settings: settingsStore, auth: authStore, key: DeriveKey(jwtKey), log: log}
}

// config reads the live secure-entry configuration.
func (g *Guard) config() (enabled bool, entryPath string) {
	if g.settings == nil {
		return false, ""
	}
	var b bool
	if raw := g.settings.Get(settings.ScopeSystem, "", "security.secure_entry_enabled"); len(raw) > 0 {
		_ = json.Unmarshal(raw, &b)
	}
	var p string
	if raw := g.settings.Get(settings.ScopeSystem, "", "security.secure_entry_path"); len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	return b, p
}

// issueValue builds the signed cookie value for an entry path.
func (g *Guard) issueValue(entryPath string) string {
	m := hmac.New(sha256.New, g.key)
	m.Write([]byte(entryPath))
	return entryPath + "." + hex.EncodeToString(m.Sum(nil))
}

// validCookie reports whether the request carries a cookie proving a visit
// to the currently configured entry path.
func (g *Guard) validCookie(c *gin.Context, wantPath string) bool {
	if wantPath == "" {
		return false
	}
	cv, err := c.Cookie(CookieName)
	if err != nil || cv == "" {
		return false
	}
	want := g.issueValue(wantPath)
	// Constant-time compare: the cookie is attacker-controlled input.
	if len(cv) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cv), []byte(want)) == 1
}

// validJWT reports whether the request carries a currently valid session
// token (same extraction rules as auth.Middleware: Bearer header or ?token=).
func (g *Guard) validJWT(c *gin.Context) bool {
	if g.auth == nil {
		return false
	}
	tokenStr := ""
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tokenStr = strings.TrimPrefix(h, "Bearer ")
	} else if q := c.Query("token"); q != "" {
		tokenStr = q
	}
	if tokenStr == "" {
		return false
	}
	_, err := g.auth.Validate(tokenStr)
	return err == nil
}

// exemptPath lists infrastructure paths that must stay reachable without an
// entry cookie: agent install/enroll (B.4 public enroll design), per-app
// webhook tokens, terminal share tokens, and the entry endpoints themselves.
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

// Middleware is the secure-entry gate. When disabled it is a no-op.
// When enabled, requests without a valid JWT, without a valid entry cookie
// and outside the exempt paths get a neutral 404.
func (g *Guard) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		enabled, entryPath := g.config()
		if !enabled {
			c.Next()
			return
		}
		p := c.Request.URL.Path
		if exemptPath(p) {
			c.Next()
			return
		}
		if g.validJWT(c) {
			c.Next()
			return
		}
		if g.validCookie(c, entryPath) {
			c.Next()
			return
		}
		// Neutral 404: do not reveal that a panel (or an API) is here.
		c.AbortWithStatus(http.StatusNotFound)
	}
}

// RegisterRoutes mounts the entry issuance endpoint. It is public by
// design: the entry path itself is the secret.
func (g *Guard) RegisterRoutes(v1 *gin.RouterGroup) {
	v1.GET("/secure-entry/:entryPath", g.handleIssue)
}

// handleIssue validates the secret entry path from the URL, sets the signed
// entry cookie and redirects to the panel root. Wrong paths get a neutral
// 404 (no oracle: identical response for disabled / wrong / right-but-... ).
func (g *Guard) handleIssue(c *gin.Context) {
	enabled, entryPath := g.config()
	got := c.Param("entryPath")
	if !enabled || entryPath == "" || len(got) != len(entryPath) ||
		subtle.ConstantTimeCompare([]byte(got), []byte(entryPath)) != 1 {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(CookieName, g.issueValue(entryPath), cookieMaxAge, "/", "", false, true)
	g.log.Info("secure entry visited, cookie issued", "remote", c.ClientIP())
	c.Redirect(http.StatusFound, "/")
}
