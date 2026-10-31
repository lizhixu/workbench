package githubapp

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"watchman/server/internal/settings"
)

// newBaseURLContext builds a context whose request looks like an operator
// browsing the panel over the LAN, i.e. the fallback address is not one
// GitHub could reach.
func newBaseURLContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "http://10.0.0.5:18789/api/v1/github-app/status", nil)
	return c
}

func TestPanelBaseURLFromBoundDomain(t *testing.T) {
	st, err := settings.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	domain, _ := json.Marshal("panel.example.com")
	if err := st.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": domain,
	}); err != nil {
		t.Fatal(err)
	}

	h := &Handlers{settings: st}
	got, configured := h.panelBaseURL(newBaseURLContext())
	if want := "http://panel.example.com"; got != want {
		t.Fatalf("base url = %q, want %q", got, want)
	}
	// A bound domain is an explicit public address, so the create page must
	// not warn about an unreachable event URL anymore.
	if !configured {
		t.Fatal("bound panel domain should count as a configured public address")
	}

	// Enabling panel SSL flips the derived scheme to https.
	enabled, _ := json.Marshal(true)
	if err := st.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_enabled": enabled,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.panelBaseURL(newBaseURLContext()); got != "https://panel.example.com" {
		t.Fatalf("ssl base url = %q, want https://panel.example.com", got)
	}
}

// A reverse proxy may terminate TLS while the panel itself runs plain HTTP.
// The bound domain must follow the scheme the browser actually used
// (X-Forwarded-Proto), otherwise the endpoint we hand to GitHub is
// http://domain and delivery fails without a redirect being followed.
func TestPanelBaseURLDomainFollowsProxyTLS(t *testing.T) {
	st, err := settings.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	domain, _ := json.Marshal("panel.example.com")
	if err := st.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": domain,
	}); err != nil {
		t.Fatal(err)
	}

	h := &Handlers{settings: st}
	c := newBaseURLContext()
	c.Request.Header.Set("X-Forwarded-Proto", "https")
	got, configured := h.panelBaseURL(c)
	if want := "https://panel.example.com"; got != want {
		t.Fatalf("base url = %q, want %q", got, want)
	}
	if !configured {
		t.Fatal("bound panel domain should count as a configured public address")
	}
}

func TestPanelBaseURLFallsBackToRequest(t *testing.T) {
	st, err := settings.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{settings: st}
	got, configured := h.panelBaseURL(newBaseURLContext())
	if want := "http://10.0.0.5:18789"; got != want {
		t.Fatalf("base url = %q, want %q", got, want)
	}
	if configured {
		t.Fatal("request-derived address must stay marked as unconfigured")
	}
}
