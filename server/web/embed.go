// Package web embeds the built web console into the server binary and
// serves it with history-mode SPA fallback (see DESIGN.md §8.1).
//
// Build contract: run `npm run build` in web/ and copy web/dist/ into
// server/web/dist/ before `go build ./server/...` (Makefile target
// build-server-with-ui and .github/workflows/release.yml do this).
// A dev binary built without the copy serves 503 for page requests;
// the API is unaffected.
//
// Access gating (secure entry) is NOT done here: the global
// secentry.Guard middleware already 404s unauthenticated requests to
// these paths when the entry is enabled, so this handler is a pure
// static file server.
package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed all:dist
var distFS embed.FS

func init() {
	// Types the stdlib mime DB may miss on minimal installs.
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".css", "text/css; charset=utf-8")
	_ = mime.AddExtensionType(".svg", "image/svg+xml")
	_ = mime.AddExtensionType(".wasm", "application/wasm")
	_ = mime.AddExtensionType(".map", "application/json")
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Handler serves the embedded console.
type Handler struct {
	fsys  fs.FS
	index []byte
	hasUI bool
}

// NewHandler builds a Handler over the embedded dist.
func NewHandler() *Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return &Handler{}
	}
	return newHandler(sub)
}

// newHandler is the test seam: unit tests inject an fstest.MapFS.
func newHandler(fsys fs.FS) *Handler {
	h := &Handler{fsys: fsys}
	if index, err := fs.ReadFile(fsys, "index.html"); err == nil {
		h.index = index
		h.hasUI = true
	}
	return h
}

// RegisterRoutes mounts the console. Call after all API routes: unknown
// /api/* paths keep a JSON 404, everything else falls back to index.html
// (Vue Router history mode).
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.GET("/", h.serveIndex)
	r.GET("/index.html", h.serveIndex)
	r.GET("/assets/*filepath", h.serveAsset)
	r.NoRoute(h.serveFallback)
}

func (h *Handler) noUI(c *gin.Context) bool {
	if h.hasUI {
		return false
	}
	c.String(http.StatusServiceUnavailable, "web console not embedded in this binary (build web/ and retry)")
	return true
}

func (h *Handler) serveIndex(c *gin.Context) {
	if h.noUI(c) {
		return
	}
	// index.html references hashed chunk names: never cache it.
	c.Header("Cache-Control", "no-cache, must-revalidate")
	c.Data(http.StatusOK, "text/html; charset=utf-8", h.index)
}

func (h *Handler) serveAsset(c *gin.Context) {
	if h.noUI(c) {
		return
	}
	rel := path.Join("assets", path.Clean("/"+c.Param("filepath")))
	data, err := fs.ReadFile(h.fsys, rel)
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	// Vite output filenames carry a content hash: immutable.
	c.Header("Cache-Control", "public, immutable, max-age=31536000")
	c.Data(http.StatusOK, contentType(rel), data)
}

func (h *Handler) serveFallback(c *gin.Context) {
	p := c.Request.URL.Path
	if strings.HasPrefix(p, "/api/") {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if h.noUI(c) {
		return
	}
	// Real files at the dist root (favicon.ico, k-works.png, ...) win;
	// everything else is a client-side route → index.html.
	if rel := sanitize(p); rel != "" && rel != "index.html" {
		if data, err := fs.ReadFile(h.fsys, rel); err == nil {
			c.Data(http.StatusOK, contentType(rel), data)
			return
		}
	}
	h.serveIndex(c)
}

// sanitize cleans a URL path into a relative fs path; "" means "not a file".
func sanitize(p string) string {
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" || rel == "." || strings.Contains(rel, "..") {
		return ""
	}
	return rel
}

func contentType(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
