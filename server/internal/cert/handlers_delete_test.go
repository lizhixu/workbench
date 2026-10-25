package cert

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestDeleteCertPanelGuard pins the panel-binding delete guard: a certificate
// explicitly bound to the panel must be refused (409) even with ?force=true,
// because deleting it would make panelsec silently fall back to another
// certificate (or fail outright once the hub is empty). Unbinding must happen
// in the panel security settings first; an unbound certificate deletes
// normally.
func TestDeleteCertPanelGuard(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.StoreCertForTest("crt_panel", []string{"panel.example.com"}, "CERT", "KEY"); err != nil {
		t.Fatal(err)
	}
	if err := h.StoreCertForTest("crt_free", []string{"free.example.com"}, "CERT", "KEY"); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandlers(h, func() (string, string) { return "crt_panel", "panel.example.com" }, nil).
		Register(r.Group(""), r.Group(""))

	del := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	if w := del("/certs/crt_panel"); w.Code != http.StatusConflict {
		t.Errorf("panel-bound delete: status = %d, want 409 (body %s)", w.Code, w.Body.String())
	}
	if w := del("/certs/crt_panel?force=true"); w.Code != http.StatusConflict {
		t.Errorf("panel-bound force delete: status = %d, want 409 (body %s)", w.Code, w.Body.String())
	}
	if _, ok := h.Get("crt_panel"); !ok {
		t.Errorf("panel-bound cert was deleted despite guard")
	}
	if w := del("/certs/crt_free"); w.Code != http.StatusOK {
		t.Errorf("unbound delete: status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if _, ok := h.Get("crt_free"); ok {
		t.Errorf("unbound cert was not deleted")
	}
}
