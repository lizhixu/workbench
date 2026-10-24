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

// TestFindByDomainRanking locks the deterministic selection rules:
// 1. exact SAN > wildcard
// 2. wildcard with more labels wins
// 3. longer remaining validity wins
// 4. certificate ID as tie-breaker
func TestFindByDomainRanking(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := makeTestCertPEM(t)
	now := time.Now()

	// 1. exact vs wildcard
	_ = h.storeResult("c_wild", "", []string{"*.example.com"}, certPEM, keyPEM, now, now.Add(90*24*time.Hour), "test", false)
	_ = h.storeResult("c_exact", "", []string{"api.example.com"}, certPEM, keyPEM, now, now.Add(30*24*time.Hour), "test", false)
	best, ok := h.FindByDomain("api.example.com")
	if !ok || best.ID != "c_exact" {
		t.Fatalf("exact must win over wildcard regardless of expiry: got %v", best)
	}

	// 2. wildcard with more labels wins
	_ = h.storeResult("c_wild_broad", "", []string{"*.shop.org"}, certPEM, keyPEM, now, now.Add(90*24*time.Hour), "test", false)
	_ = h.storeResult("c_wild_specific", "", []string{"*.sub.shop.org"}, certPEM, keyPEM, now, now.Add(30*24*time.Hour), "test", false)
	best, ok = h.FindByDomain("api.sub.shop.org")
	if !ok || best.ID != "c_wild_specific" {
		t.Fatalf("more specific wildcard must win: got %v", best)
	}

	// 3. longer remaining validity wins between equal wildcards
	_ = h.storeResult("c_w1", "", []string{"*.domain.com"}, certPEM, keyPEM, now, now.Add(30*24*time.Hour), "test", false)
	_ = h.storeResult("c_w2", "", []string{"*.domain.com"}, certPEM, keyPEM, now, now.Add(60*24*time.Hour), "test", false)
	best, ok = h.FindByDomain("test.domain.com")
	if !ok || best.ID != "c_w2" {
		t.Fatalf("longer validity must win: got %v", best)
	}
}

// TestStoreResultPreservesRemark proves that re-storing a certificate (renewal)
// retains the admin-managed remark.
func TestStoreResultPreservesRemark(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := makeTestCertPEM(t)
	now := time.Now()
	if err := h.storeResult("c_rem", "", []string{"remark.example.com"}, certPEM, keyPEM, now, now.Add(30*24*time.Hour), "test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.UpdateRemark("c_rem", "生产环境核心证书"); err != nil {
		t.Fatal(err)
	}
	// Renewal arrives with updated expiry:
	if err := h.storeResult("c_rem", "", []string{"remark.example.com"}, certPEM, keyPEM, now, now.Add(90*24*time.Hour), "test", false); err != nil {
		t.Fatal(err)
	}
	got, ok := h.Get("c_rem")
	if !ok {
		t.Fatal("c_rem missing")
	}
	if got.Remark != "生产环境核心证书" {
		t.Fatalf("expected remark preserved, got %q", got.Remark)
	}
}

// TestDeleteCertGuardedByApps: deleting a certificate bound to an app returns
// 409 Conflict, but force=true clears the bindings and succeeds.
func TestDeleteCertGuardedByApps(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := makeTestCertPEM(t)
	now := time.Now()
	_ = h.storeResult("crt_bound", "", []string{"app.example.com"}, certPEM, keyPEM, now, now.Add(30*24*time.Hour), "test", false)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	clearedID := ""
	handlers := NewHandlers(h, nil, func() []AppDomainRef {
		return []AppDomainRef{
			{ID: "app1", Name: "WebApp", Domain: "app.example.com", CertID: "crt_bound"},
		}
	})
	handlers.SetAppCertBindingClearer(func(certID string) []string {
		clearedID = certID
		return []string{"WebApp"}
	})
	handlers.Register(r.Group(""), r.Group(""))

	// Normal delete -> 409 Conflict
	req := httptest.NewRequest(http.MethodDelete, "/certs/crt_bound", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "WebApp") {
		t.Fatalf("expected error mentioning WebApp, got %s", w.Body.String())
	}

	// Force delete -> 200 OK + callback called
	reqForce := httptest.NewRequest(http.MethodDelete, "/certs/crt_bound?force=true", nil)
	wForce := httptest.NewRecorder()
	r.ServeHTTP(wForce, reqForce)
	if wForce.Code != http.StatusOK {
		t.Fatalf("force delete code = %d, want 200, body = %s", wForce.Code, wForce.Body.String())
	}
	if clearedID != "crt_bound" {
		t.Fatalf("expected clearedID = crt_bound, got %q", clearedID)
	}
	if _, ok := h.Get("crt_bound"); ok {
		t.Fatal("certificate should be deleted after force delete")
	}
}
