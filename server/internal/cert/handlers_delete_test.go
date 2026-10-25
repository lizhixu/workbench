package cert

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestDeleteCertPanelGuard pins the panel-binding delete guard: a certificate
// actively served by the panel (SSL enabled, cert_center mode) must be
// refused (409) even with ?force=true, because deleting it would make
// panelsec silently fall back to another certificate (or fail outright once
// the hub is empty). Unbinding must happen in the panel security settings
// first; an unbound certificate deletes normally, and so does a stale
// SSLCertID left behind after switching to custom PEM or disabling SSL —
// the panel no longer serves that certificate, so it must not block the
// delete.
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
	NewHandlers(h, func() PanelCertBinding {
		return PanelCertBinding{CertID: "crt_panel", Domain: "panel.example.com", Active: true}
	}, nil).Register(r.Group(""), r.Group(""))

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

// TestDeleteCertPanelGuardInactiveBinding pins the guard's Active condition:
// a stale SSLCertID under a disabled-SSL or custom-PEM panel must not block
// the delete, since the panel serves no certificate from the hub in either
// state. The force parameter stays irrelevant here — the delete succeeds
// without it.
func TestDeleteCertPanelGuardInactiveBinding(t *testing.T) {
	for _, active := range []struct {
		name    string
		binding PanelCertBinding
	}{
		{"ssl disabled", PanelCertBinding{CertID: "crt_stale", Domain: "panel.example.com", Active: false}},
		{"custom PEM mode", PanelCertBinding{CertID: "crt_stale", Domain: "panel.example.com", Active: false}},
	} {
		t.Run(active.name, func(t *testing.T) {
			h, err := NewHub(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.StoreCertForTest("crt_stale", []string{"panel.example.com"}, "CERT", "KEY"); err != nil {
				t.Fatal(err)
			}
			gin.SetMode(gin.TestMode)
			r := gin.New()
			binding := active.binding
			NewHandlers(h, func() PanelCertBinding { return binding }, nil).
				Register(r.Group(""), r.Group(""))
			req := httptest.NewRequest(http.MethodDelete, "/certs/crt_stale", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("stale panel cert delete: status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if _, ok := h.Get("crt_stale"); ok {
				t.Errorf("stale panel cert was not deleted")
			}
		})
	}
}
