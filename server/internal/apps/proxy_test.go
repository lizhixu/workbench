package apps

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/cert"
	"watchman/server/internal/rpc"
)

// TestResolveGatewayUpstream: 异地组网是网关模式的其中一条路径而非前置条件——
// 双方都在组网内走 100.x，否则直连应用主机的公网 IP，再回退内网 IP；绝不用
// 127.0.0.1（会指向网关自身造成流量黑洞）。
func TestResolveGatewayUpstream(t *testing.T) {
	cases := []struct {
		name                       string
		appMesh, gwMesh            string
		appPublicIP, appInternalIP string
		want                       string
		wantErr                    bool
	}{
		{
			name:    "both in mesh prefers overlay",
			appMesh: "100.64.0.5", gwMesh: "100.64.0.1",
			appPublicIP: "1.2.3.4", appInternalIP: "10.0.0.5",
			want: "100.64.0.5:8080",
		},
		{
			name:    "app in mesh but gateway not falls back to direct public",
			appMesh: "100.64.0.5", gwMesh: "",
			appPublicIP: "1.2.3.4", appInternalIP: "10.0.0.5",
			want: "1.2.3.4:8080",
		},
		{
			name:    "neither in mesh uses public IP",
			appMesh: "", gwMesh: "",
			appPublicIP: "1.2.3.4", appInternalIP: "10.0.0.5",
			want: "1.2.3.4:8080",
		},
		{
			name:    "no public IP falls back to internal",
			appMesh: "", gwMesh: "",
			appPublicIP: "", appInternalIP: "10.0.0.5",
			want: "10.0.0.5:8080",
		},
		{
			name:    "nothing resolvable errors instead of 127.0.0.1",
			appMesh: "", gwMesh: "",
			appPublicIP: "", appInternalIP: "",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		got, err := resolveGatewayUpstream(8080, tc.appMesh, tc.gwMesh, tc.appPublicIP, tc.appInternalIP)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got upstream %q", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if got == "127.0.0.1:8080" {
			t.Errorf("%s: upstream must never be loopback", tc.name)
		}
	}
}

// TestValidProxyDomain: the domain is interpolated verbatim into a root-
// written nginx server block, so anything outside strict hostname shape
// must be rejected.
func TestValidProxyDomain(t *testing.T) {
	valid := []string{
		"example.com",
		"api.example.com",
		"a-b.example.com",
		"app1.internal",
		"localhost",
		"x.co",
	}
	for _, d := range valid {
		if !validProxyDomain(d) {
			t.Errorf("validProxyDomain(%q) = false, want true", d)
		}
	}
	invalid := []string{
		"",
		"example.com; rm -rf /",
		"example.com\nserver_name evil",
		"exa mple.com",
		"example.com}",
		"${host}.example.com",
		"-lead.example.com",
		"trail-.example.com",
		"double..dot.com",
		".leading.com",
		"trailing.com.",
		"under_score.com",
		"UPPER.com", // caller lowercases first; raw upper is rejected
	}
	for _, d := range invalid {
		if validProxyDomain(d) {
			t.Errorf("validProxyDomain(%q) = true, want false", d)
		}
	}
}

// TestAppsCoveredByCert: renewal re-push target selection — a freshly stored
// certificate must reach every app whose bound domain it covers (exact or
// wildcard SAN, the same cert.DomainCovers rule FindByDomain applies when
// binding), and the nginx host is resolved per proxy mode: local → the app
// host, gateway → the recorded gateway host (app host as fallback).
func TestAppsCoveredByCert(t *testing.T) {
	crt := &cert.Certificate{Domains: []string{"api.example.com", "*.shop.org"}}
	apps := []*Application{
		{ID: "a1", Name: "local", HostID: "h1", Domain: "api.example.com", ProxyMode: "local"},
		{ID: "a2", Name: "gateway", HostID: "h2", Domain: "www.shop.org", ProxyMode: "gateway", ProxyGatewayHostID: "gw1"},
		{ID: "a3", Name: "unbound", HostID: "h3", Domain: "", ProxyMode: "local"},
		{ID: "a4", Name: "unrelated", HostID: "h4", Domain: "other.org", ProxyMode: "local"},
		{ID: "a5", Name: "wildcard-apex", HostID: "h5", Domain: "shop.org", ProxyMode: "local"},
		{ID: "a6", Name: "gateway-no-host", HostID: "h6", Domain: "api.example.com", ProxyMode: "gateway", ProxyGatewayHostID: ""},
	}
	got := appsCoveredByCert(crt, apps)
	if len(got) != 3 {
		t.Fatalf("selected %d apps, want 3: %+v", len(got), got)
	}
	byID := map[string]repushTarget{}
	for _, t := range got {
		byID[t.app.ID] = t
	}
	if byID["a1"].targetHost != "h1" {
		t.Errorf("local mode must target the app host, got %q", byID["a1"].targetHost)
	}
	if byID["a2"].targetHost != "gw1" {
		t.Errorf("gateway mode must target the gateway host, got %q", byID["a2"].targetHost)
	}
	if byID["a2"].domain != "www.shop.org" {
		t.Errorf("domain must be normalized as bound, got %q", byID["a2"].domain)
	}
	if byID["a6"].targetHost != "h6" {
		t.Errorf("gateway without a recorded host falls back to the app host, got %q", byID["a6"].targetHost)
	}
	for _, id := range []string{"a3", "a4", "a5"} {
		if _, ok := byID[id]; ok {
			t.Errorf("app %s must not be selected (empty/unrelated/wildcard-apex domain)", id)
		}
	}
	// A nil certificate selects nothing.
	if appsCoveredByCert(nil, apps) != nil {
		t.Error("nil certificate must select no apps")
	}
}

// TestRepushAppCertsGuards: the hub's OnRenewed callback fires with whatever
// wiring exists — nil cert / missing PEM / missing deps must all be silent
// no-ops instead of panicking inside the notify goroutine.
func TestRepushAppCertsGuards(t *testing.T) {
	ph := NewProxyHandler(nil, nil, nil)
	ph.RepushAppCerts(nil)
	ph.RepushAppCerts(&cert.Certificate{Domains: []string{"a.example.com"}})
	ph.RepushAppCerts(&cert.Certificate{Domains: []string{"a.example.com"}, CertPEM: "x", KeyPEM: "y"})
	ph.RepushAppCerts(&cert.Certificate{Domains: []string{"a.example.com"}, CertPEM: "x", KeyPEM: "y"})
}

// TestUnbindProxyClearsGatewayHostID: unbinding must wipe the whole binding
// — the gateway host id included, or a stale id would survive into the next
// bind and mislead the app detail UI (and the renewal re-push host lookup).
func TestUnbindProxyClearsGatewayHostID(t *testing.T) {
	store := newTestStore(t)
	app := &Application{
		ID:                 "app_gw",
		Name:               "demo",
		HostID:             "h1",
		Domain:             "www.example.com",
		ProxyMode:          "gateway",
		ProxyUpstream:      "127.0.0.1:8080",
		ProxyGatewayHostID: "gw1",
		Ports:              []PortMapping{{Host: 8080, Container: 80}},
	}
	if err := store.PutApp(app); err != nil {
		t.Fatalf("PutApp: %v", err)
	}
	// Offline registry: Hub() returns nil for every host, so the conf
	// removal loop skips — exactly the path a test environment exercises.
	reg := rpc.NewRegistry(t.TempDir(), nil)
	ph := NewProxyHandler(reg, store, nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	ph.Register(r.Group(""))
	req := httptest.NewRequest(http.MethodDelete, "/apps/app_gw/proxy", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unbind: code = %d, body %s", w.Code, w.Body.String())
	}
	got, ok := store.GetApp("app_gw")
	if !ok {
		t.Fatal("app disappeared on unbind")
	}
	if got.Domain != "" || got.ProxyMode != "" || got.ProxyUpstream != "" || got.ProxyGatewayHostID != "" {
		t.Fatalf("unbind must clear the whole binding incl. gateway host: %+v", got)
	}
}
