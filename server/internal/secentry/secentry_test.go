package secentry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"watchman/server/internal/auth"
	"watchman/server/internal/settings"
)

func testGuard(t *testing.T) (*Guard, *settings.Store, *auth.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	ss, err := settings.NewStore(dir, nil)
	if err != nil {
		t.Fatalf("settings store: %v", err)
	}
	as, err := auth.NewStore(dir, "test-jwt-key")
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	return NewGuard(ss, as, as.SigningKey(), nil), ss, as
}

func setEntry(t *testing.T, ss *settings.Store, enabled bool, path string) {
	t.Helper()
	raw := func(v any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	if err := ss.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.secure_entry_enabled": raw(enabled),
		"security.secure_entry_path":    raw(path),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
}

// testEngine wires the guard exactly like api.Router does: middleware first,
// then routes.
func testEngine(g *Guard) *gin.Engine {
	r := gin.New()
	r.Use(g.Middleware())
	v1 := r.Group("/api/v1")
	g.RegisterRoutes(v1)
	v1.POST("/auth/login", func(c *gin.Context) { c.Status(http.StatusOK) })
	v1.GET("/hosts", func(c *gin.Context) { c.Status(http.StatusOK) })
	v1.POST("/hosts/enroll", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func doReq(t *testing.T, r *gin.Engine, method, path string, headers map[string]string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGuardDisabledPasses(t *testing.T) {
	g, ss, _ := testGuard(t)
	setEntry(t, ss, false, "")
	r := testEngine(g)
	if w := doReq(t, r, "POST", "/api/v1/auth/login", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("disabled: login got %d, want 200", w.Code)
	}
}

func TestGuardBlocksUnauthenticated(t *testing.T) {
	g, ss, _ := testGuard(t)
	setEntry(t, ss, true, "k9xQ2mZ7aB4cD8eF")
	r := testEngine(g)
	for _, p := range []string{"/api/v1/auth/login", "/api/v1/hosts"} {
		if w := doReq(t, r, "GET", p, nil, nil); w.Code != http.StatusNotFound {
			t.Fatalf("enabled, no cookie: %s got %d, want 404", p, w.Code)
		}
	}
}

func TestEntryIssueAndCookie(t *testing.T) {
	g, ss, _ := testGuard(t)
	setEntry(t, ss, true, "k9xQ2mZ7aB4cD8eF")
	r := testEngine(g)

	// Wrong path: neutral 404, no cookie.
	w := doReq(t, r, "GET", "/api/v1/secure-entry/wrongpath123456", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("wrong path got %d, want 404", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("wrong path must not set a cookie")
	}

	// Right path: 302 + signed cookie.
	w = doReq(t, r, "GET", "/api/v1/secure-entry/k9xQ2mZ7aB4cD8eF", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("right path got %d, want 302", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != CookieName {
		t.Fatalf("expected %s cookie, got %v", CookieName, cookies)
	}
	if !cookies[0].HttpOnly {
		t.Fatal("entry cookie must be HttpOnly")
	}

	// With cookie: login endpoint reachable.
	w = doReq(t, r, "POST", "/api/v1/auth/login", nil, cookies)
	if w.Code != http.StatusOK {
		t.Fatalf("with cookie: login got %d, want 200", w.Code)
	}

	// Forged cookie: 404.
	forged := &http.Cookie{Name: CookieName, Value: "k9xQ2mZ7aB4cD8eF.forged"}
	w = doReq(t, r, "POST", "/api/v1/auth/login", nil, []*http.Cookie{forged})
	if w.Code != http.StatusNotFound {
		t.Fatalf("forged cookie got %d, want 404", w.Code)
	}
}

func TestGuardJWTBypass(t *testing.T) {
	g, ss, as := testGuard(t)
	setEntry(t, ss, true, "k9xQ2mZ7aB4cD8eF")
	if _, err := as.Create("entryop", "Qa#12345678", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	tok, _, err := as.Authenticate("entryop", "Qa#12345678")
	if err != nil {
		t.Fatal(err)
	}
	r := testEngine(g)
	h := map[string]string{"Authorization": "Bearer " + tok}
	if w := doReq(t, r, "GET", "/api/v1/hosts", h, nil); w.Code != http.StatusOK {
		t.Fatalf("valid JWT got %d, want 200", w.Code)
	}
}

func TestGuardExemptions(t *testing.T) {
	g, ss, _ := testGuard(t)
	setEntry(t, ss, true, "k9xQ2mZ7aB4cD8eF")
	r := gin.New()
	r.Use(g.Middleware())
	v1 := r.Group("/api/v1")
	g.RegisterRoutes(v1)
	v1.POST("/hosts/enroll", func(c *gin.Context) { c.Status(http.StatusOK) })
	v1.POST("/apps/webhook/:token", func(c *gin.Context) { c.Status(http.StatusOK) })
	v1.GET("/apps/webhook/:token", func(c *gin.Context) { c.Status(http.StatusOK) })
	v1.GET("/terminals/share/:token", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/install", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/agent/binary", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/hosts/enroll"},
		{"GET", "/api/v1/apps/webhook/sometoken"},
		{"GET", "/api/v1/terminals/share/sometoken"},
		{"GET", "/install"},
		{"GET", "/agent/binary"},
		{"GET", "/api/v1/secure-entry/check"},
	} {
		if w := doReq(t, r, tc.method, tc.path, nil, nil); w.Code == http.StatusNotFound {
			t.Fatalf("exempt path %s must not be 404 (got %d)", tc.path, w.Code)
		}
	}
}

func TestCheckEndpoint(t *testing.T) {
	g, ss, _ := testGuard(t)
	r := testEngine(g)

	// Disabled -> 200.
	setEntry(t, ss, false, "")
	if w := doReq(t, r, "GET", "/api/v1/secure-entry/check", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("disabled check got %d, want 200", w.Code)
	}

	// Enabled, no cookie -> 401.
	setEntry(t, ss, true, "k9xQ2mZ7aB4cD8eF")
	if w := doReq(t, r, "GET", "/api/v1/secure-entry/check", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("enabled check without cookie got %d, want 401", w.Code)
	}

	// Enabled, with cookie -> 200.
	w := doReq(t, r, "GET", "/api/v1/secure-entry/k9xQ2mZ7aB4cD8eF", nil, nil)
	cookies := w.Result().Cookies()
	if w := doReq(t, r, "GET", "/api/v1/secure-entry/check", nil, cookies); w.Code != http.StatusOK {
		t.Fatalf("enabled check with cookie got %d, want 200", w.Code)
	}
}

func TestEntryPathValidation(t *testing.T) {
	_, ss, _ := testGuard(t)
	raw := func(v any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"", true},
		{"k9xQ2mZ7aB4cD8eF", true},
		{"aBcDeF123456_-x", true},
		{"abc12", false},      // too short
		{"-abc123456", false}, // leading dash
		{"abc 123456", false}, // space
		{"abc/123456", false}, // slash
	} {
		err := ss.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
			"security.secure_entry_path": raw(tc.path),
		})
		if (err == nil) != tc.ok {
			t.Fatalf("SetMany path=%q: err=%v, want ok=%v", tc.path, err, tc.ok)
		}
	}
	long := ""
	for len(long) < 65 {
		long += "a1"
	}
	if err := ss.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.secure_entry_path": raw(long[:65]),
	}); err == nil {
		t.Fatal("65-char path must be rejected")
	}
	// Enabling + path persist together through the real store path.
	if err := ss.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.secure_entry_enabled": raw(true),
		"security.secure_entry_path":    raw("entryPath123456"),
	}); err != nil {
		t.Fatalf("SetMany valid: %v", err)
	}
}
