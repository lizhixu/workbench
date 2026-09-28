package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"watchman/server/internal/settings"
)

// TestSecureEntryGuardWiredInRouter verifies the secure-entry guard is
// actually registered in the real Router (not just a test double). With the
// entry enabled, the real login route must 404 without an entry cookie, and
// visiting the real entry URL must issue a cookie that unlocks it.
// This guards against regressions in middleware registration order: gin
// silently drops r.Use calls made after route registration.
func TestSecureEntryGuardWiredInRouter(t *testing.T) {
	r, _, settingsStore := newTestRouter(t)
	raw := func(v any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	if err := settingsStore.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.secure_entry_enabled": raw(true),
		"security.secure_entry_path":    raw("e2eEntryPath123"),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}

	loginBody := `{"username":"admin","password":"wrong"}`

	// 1. No cookie: the real login route is hidden (404, not 401).
	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("login without entry cookie: got %d, want 404", w.Code)
	}

	// 2. Visit the real entry URL: 302 + signed cookie.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/secure-entry/e2eEntryPath123", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("entry visit: got %d, want 302", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("entry visit did not set a cookie")
	}

	// 3. With the cookie the request reaches the real login handler:
	// wrong password now yields 401 (handler ran), not 404 (guard blocked).
	req = httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("login with entry cookie: got %d, want 401 (handler reached)", w.Code)
	}

	// 4. Wrong entry path stays hidden.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/secure-entry/nope-nope-nope-1", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("wrong entry path: got %d, want 404", w.Code)
	}

	// 5. Static pages are hidden by the same guard (no nginx involved):
	// without a cookie, GET / is 404.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("static / without entry cookie: got %d, want 404", w.Code)
	}

	// 6. With the entry cookie the request reaches the embedded web
	// handler: 200 when the UI is embedded (release builds), 503 when it
	// is not (dev checkouts) — either way the guard let it through
	// instead of 404ing it.
	req = httptest.NewRequest("GET", "/", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
		t.Fatalf("static / with entry cookie: got %d, want 200 or 503 (web handler reached)", w.Code)
	}
}
