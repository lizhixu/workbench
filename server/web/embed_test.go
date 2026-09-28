package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/app-abc123.js": &fstest.MapFile{Data: []byte("console.log(1)")},
		"favicon.ico":          &fstest.MapFile{Data: []byte("ico")},
	}
}

func testRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterRoutes(r)
	return r
}

func TestServeIndex(t *testing.T) {
	r := testRouter(newHandler(testFS()))
	for _, p := range []string{"/", "/index.html"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: got %d, want 200", p, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("GET %s: content-type %q", p, ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
			t.Errorf("GET %s: cache-control %q, index.html must not be cached", p, cc)
		}
		if w.Body.String() != "<html>app</html>" {
			t.Errorf("GET %s: unexpected body", p)
		}
	}
}

func TestServeAsset(t *testing.T) {
	r := testRouter(newHandler(testFS()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/assets/app-abc123.js", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("asset: got %d, want 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, immutable, max-age=31536000" {
		t.Errorf("asset cache-control = %q, want immutable", cc)
	}
	// Missing asset → 404, not the SPA fallback.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/assets/nope.js", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("missing asset: got %d, want 404", w.Code)
	}
}

func TestSPAFallback(t *testing.T) {
	r := testRouter(newHandler(testFS()))
	// Client-side route → index.html.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/hosts/123", nil))
	if w.Code != http.StatusOK || w.Body.String() != "<html>app</html>" {
		t.Errorf("SPA route: got %d %q", w.Code, w.Body.String())
	}
	// Real file at dist root wins over the fallback.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/favicon.ico", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ico" {
		t.Errorf("favicon: got %d %q", w.Code, w.Body.String())
	}
	// Unknown API path keeps a clean 404 (no index.html leak).
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown API: got %d, want 404", w.Code)
	}
	// Path traversal is neutralized.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/..%2f..%2fetc/passwd", nil))
	if w.Code != http.StatusOK {
		t.Errorf("traversal: got %d, want SPA fallback 200", w.Code)
	}
}

func TestNoUI(t *testing.T) {
	r := testRouter(newHandler(fstest.MapFS{}))
	for _, p := range []string{"/", "/assets/a.js", "/hosts/1"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s without embedded UI: got %d, want 503", p, w.Code)
		}
	}
}
