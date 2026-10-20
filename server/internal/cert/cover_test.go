package cert

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestValidCoverDomain: the create-page probe must only green-light domains
// the reverse-proxy binding itself would accept (apps' validProxyDomain has
// the same shape), so anything outside strict hostname form is rejected —
// including wildcards, which have to be issued in the cert center first.
func TestValidCoverDomain(t *testing.T) {
	valid := []string{
		"example.com",
		"api.example.com",
		"a-b.example.com",
		"app1.internal",
		"localhost",
	}
	for _, d := range valid {
		if !validCoverDomain(d) {
			t.Errorf("validCoverDomain(%q) = false, want true", d)
		}
	}
	invalid := []string{
		"",
		"*.example.com",
		"example.com; rm -rf /",
		"exa mple.com",
		"-lead.example.com",
		"trail-.example.com",
		"double..dot.com",
		".leading.com",
		"trailing.com.",
		"under_score.com",
		"UPPER.com", // caller lowercases first; raw upper is rejected
		strings.Repeat("a", 254) + ".com",
	}
	for _, d := range invalid {
		if validCoverDomain(d) {
			t.Errorf("validCoverDomain(%q) = true, want false", d)
		}
	}
}

// TestGetCertCoverEndpoint: the cover probe must apply hub.FindByDomain's
// matching rule (exact SAN or wildcard) and must never leak PEM material —
// the create page and the issuance poller both depend on it, and the
// response shape must agree with what bindProxy later resolves.
func TestGetCertCoverEndpoint(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// One exact-SAN cert (imported → manual_upload provenance) and one
	// wildcard cert. Their cover sets stay disjoint so the map iteration
	// order in FindByDomain cannot make assertions flaky.
	exactPEM, exactKey := makeTestCertPEM(t)
	if err := h.storeResult("crt_exact", "", []string{"api.example.com"},
		exactPEM, exactKey, time.Now(), time.Now().Add(90*24*time.Hour), "test", false); err != nil {
		t.Fatal(err)
	}
	wildPEM, wildKey := makeTestCertPEM(t)
	if err := h.storeResult("crt_wild", "acc1", []string{"*.other.org"},
		wildPEM, wildKey, time.Now(), time.Now().Add(90*24*time.Hour), "test", true); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandlers(h, nil, nil).Register(r.Group(""), r.Group(""))

	get := func(domain string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/certs/cover?domain="+domain, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Exact SAN covered.
	w := get("api.example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("exact: code = %d, body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"covered":true`, `"id":"crt_exact"`, `"source":"manual_upload"`} {
		if !strings.Contains(body, want) {
			t.Errorf("exact: body missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "BEGIN") {
		t.Errorf("exact: response must never carry PEM material: %s", body)
	}

	// Wildcard SAN covers its subdomains but not the apex.
	w = get("shop.other.org")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"crt_wild"`) {
		t.Fatalf("wildcard sub: code = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"source":"acme"`) {
		t.Errorf("wildcard sub: source should be acme, body %s", w.Body.String())
	}
	w = get("other.org")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"covered":false`) {
		t.Fatalf("wildcard apex: code = %d, body %s", w.Code, w.Body.String())
	}

	// Unrelated domain.
	w = get("nothing.example.org")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"covered":false`) {
		t.Fatalf("unrelated: code = %d, body %s", w.Code, w.Body.String())
	}

	// Invalid / empty / wildcard domains → 400, never a probe result.
	for _, d := range []string{"*.example.com", "exa+ple.com", ""} {
		w = get(d)
		if w.Code != http.StatusBadRequest {
			t.Errorf("invalid %q: code = %d, want 400", d, w.Code)
		}
	}
}

// TestNotifyRenewedCallback: every storeResult success path (new issuance,
// renewal, manual confirm — Import stands in here) must hand the stored
// certificate — PEMs included, the re-push needs them — to the OnRenewed
// hook on its own goroutine. A nil hook and a panicking hook must both be
// tolerated without breaking the issuing flow.
func TestNotifyRenewedCallback(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// No hook set: storing stays silent and safe.
	certPEM, keyPEM := makeTestCertPEM(t)
	if _, err := h.Import(certPEM, keyPEM); err != nil {
		t.Fatalf("Import without hook: %v", err)
	}

	// Hook set: the callback receives the stored cert with PEM material.
	got := make(chan *Certificate, 1)
	h.SetOnRenewed(func(c *Certificate) {
		if c == nil || c.CertPEM == "" || c.KeyPEM == "" {
			t.Error("callback must receive a PEM-carrying certificate")
		}
		got <- c
	})
	imported, err := h.Import(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Import with hook: %v", err)
	}
	select {
	case c := <-got:
		if c.ID != imported.ID {
			t.Errorf("callback id = %q, want %q", c.ID, imported.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback was not invoked after Import")
	}

	// Panicking hook: recovered inside notifyRenewed's goroutine, the
	// issuance flow itself must complete normally.
	h.SetOnRenewed(func(*Certificate) { panic("boom") })
	if _, err := h.Import(certPEM, keyPEM); err != nil {
		t.Fatalf("Import with panicking hook must not fail: %v", err)
	}

	// Nil hook restores the no-op behavior.
	h.SetOnRenewed(nil)
	if _, err := h.Import(certPEM, keyPEM); err != nil {
		t.Fatalf("Import with nil hook: %v", err)
	}
}
