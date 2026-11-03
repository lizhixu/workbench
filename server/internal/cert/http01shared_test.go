package cert

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServeSharedHTTP01Scope pins the responder's contract: it only claims
// GET/HEAD requests under /.well-known/acme-challenge/ with a token that is
// actually registered; everything else falls through to the caller so the
// panel keeps serving it normally.
func TestServeSharedHTTP01Scope(t *testing.T) {
	// The shared registry is package-global; keep the test hermetic.
	t.Cleanup(func() { clearSharedHTTP01Token("tok-1") })

	setSharedHTTP01Token("tok-1", "tok-1.keyauth")

	answered := func(method, path string) (*httptest.ResponseRecorder, bool) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		return w, ServeSharedHTTP01(w, req)
	}

	if w, ok := answered(http.MethodGet, "/.well-known/acme-challenge/tok-1"); !ok {
		t.Fatal("registered token must be answered")
	} else if w.Code != http.StatusOK || w.Body.String() != "tok-1.keyauth" {
		t.Fatalf("answer = %d %q, want 200 keyauth", w.Code, w.Body.String())
	}
	if w, ok := answered(http.MethodHead, "/.well-known/acme-challenge/tok-1"); !ok || w.Code != http.StatusOK {
		t.Fatalf("HEAD handling: ok=%v code=%d", ok, w.Code)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/.well-known/acme-challenge/tok-1"}, // only GET/HEAD
		{http.MethodGet, "/.well-known/acme-challenge/"},        // empty token
		{http.MethodGet, "/.well-known/acme-challenge/a/b"},     // nested path
		{http.MethodGet, "/.well-known/acme-challenge/unknown"}, // unregistered
		{http.MethodGet, "/api/v1/certs"},                       // unrelated path
	} {
		if _, ok := answered(tc.method, tc.path); ok {
			t.Errorf("%s %q must not be claimed", tc.method, tc.path)
		}
	}

	clearSharedHTTP01Token("tok-1")
	if _, ok := answered(http.MethodGet, "/.well-known/acme-challenge/tok-1"); ok {
		t.Fatal("token must be gone after cleanup")
	}
}

// TestBuildSolversSharedHTTP01Fallback pins buildSolvers' probe behavior: when
// the temporary :80 bind fails but an external stack is serving :80, the
// http-01 solver must stay usable (a domain-bound panel owns :80 itself); with
// no external stack the solver stays unavailable as before.
func TestBuildSolversSharedHTTP01Fallback(t *testing.T) {
	// Occupy the port so the temporary listener cannot bind.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve test port: %v", err)
	}
	defer blocker.Close()

	oldAddr := http01ListenAddr
	http01ListenAddr = blocker.Addr().String()
	defer func() { http01ListenAddr = oldAddr }()

	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// No probe wired: unavailable, exactly like before the shared responder.
	s := h.buildSolvers()
	s.stop()
	if s.hasAny() {
		t.Fatal("solver set must be empty when the bind fails and no external stack serves :80")
	}

	SetSharedHTTP01Probe(func() bool { return true })
	defer SetSharedHTTP01Probe(nil)

	s = h.buildSolvers()
	defer s.stop()
	if s.http == nil {
		t.Fatal("http-01 solver must stay usable while the external stack serves :80")
	}
}