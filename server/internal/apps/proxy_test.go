package apps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
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

// TestClassifyEnsureError: ensure failures the user can fix on the host (no
// docker / 80-443 occupied / pull failed) must map to their action hints;
// everything else must stay a non-sentinel error carrying a log excerpt so
// nothing is silently swallowed.
func TestClassifyEnsureError(t *testing.T) {
	sentinels := []error{errProxyDockerUnavailable, errProxyPortConflict, errProxyImagePull}
	cases := []struct {
		name        string
		exit        int32
		stdout      string
		stderr      string
		want        error
		wantExcerpt string
	}{
		{
			name: "docker binary missing",
			exit: proxyEnsureExitNoDocker, want: errProxyDockerUnavailable,
		},
		{
			name:   "daemon unreachable",
			exit:   proxyEnsureExitDaemonDown,
			stderr: "Cannot connect to the Docker daemon",
			want:   errProxyDockerUnavailable,
		},
		{
			name:   "pull access denied",
			exit:   proxyEnsureExitRunFailed,
			stderr: "Unable to find image 'nginx:alpine' locally\ndocker: pull access denied for nginx",
			want:   errProxyImagePull,
		},
		{
			name:   "registry timeout",
			exit:   proxyEnsureExitRunFailed,
			stderr: `docker: Error response from daemon: Get "https://registry-1.docker.io/v2/": dial tcp: i/o timeout`,
			want:   errProxyImagePull,
		},
		{
			name:   "port 80 conflict from nginx logs",
			exit:   proxyEnsureExitNotRunning,
			stdout: "STATUS=restarting\n2026/10/20 [emerg] bind() to 0.0.0.0:80 failed (98: Address already in use)",
			want:   errProxyPortConflict,
		},
		{
			name:   "port 443 conflict from nginx logs",
			exit:   proxyEnsureExitNotRunning,
			stdout: "STATUS=exited\nbind() to 0.0.0.0:443 failed (98: Address already in use)",
			want:   errProxyPortConflict,
		},
		{
			name:        "container death for another reason stays generic with log excerpt",
			exit:        proxyEnsureExitNotRunning,
			stdout:      "STATUS=exited\nnginx: [emerg] host not found in upstream",
			wantExcerpt: "host not found in upstream",
		},
		{
			name:        "docker run failure unrelated to pull stays generic",
			exit:        proxyEnsureExitRunFailed,
			stderr:      "docker: error response from daemon: driver failed",
			wantExcerpt: "driver failed",
		},
		{
			name:        "mkdir permission denied stays generic",
			exit:        proxyEnsureExitMkdirFailed,
			stderr:      "mkdir: cannot create directory '/opt/apps/nginx': Permission denied",
			wantExcerpt: "Permission denied",
		},
		{
			name:        "no output at all still errors with exit code",
			exit:        7,
			wantExcerpt: "exit 7",
		},
	}
	for _, tc := range cases {
		err := classifyEnsureError(tc.exit, tc.stdout, tc.stderr)
		if err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
			continue
		}
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: got %q, want sentinel %q", tc.name, err, tc.want)
			}
			continue
		}
		for _, s := range sentinels {
			if errors.Is(err, s) {
				t.Errorf("%s: generic failure must not classify as %q, got %q", tc.name, s, err)
			}
		}
		if tc.wantExcerpt != "" && !strings.Contains(err.Error(), tc.wantExcerpt) {
			t.Errorf("%s: error must carry the log excerpt %q, got %q", tc.name, tc.wantExcerpt, err)
		}
	}
	// Long logs are truncated so the API response stays readable.
	long := strings.Repeat("x", 400)
	err := classifyEnsureError(proxyEnsureExitNotRunning, long, "")
	if len(err.Error()) > 260 {
		t.Errorf("log excerpt must be truncated to ~200 chars, got %d", len(err.Error()))
	}
}

// TestProxyEnsureScripts: the check/run scripts must carry the exact container
// name, both :ro mounts, host networking and the managed label — a drift here
// is the difference between a healthy proxy and a silent traffic blackhole.
func TestProxyEnsureScripts(t *testing.T) {
	check := proxyEnsureCheckScript()
	run := proxyEnsureRunScript()
	for _, s := range []string{check, run} {
		if !strings.Contains(s, nginxContainer) {
			t.Errorf("script must reference the container const %q:\n%s", nginxContainer, s)
		}
	}
	for _, want := range []string{
		"mkdir -p " + proxyConfigDir + " " + proxyCertDir,
		"command -v docker",
		"docker info",
		"{{.State.Status}}",
		"{{range .Mounts}}",
	} {
		if !strings.Contains(check, want) {
			t.Errorf("check script must contain %q:\n%s", want, check)
		}
	}
	for _, want := range []string{
		"docker rm -f " + nginxContainer,
		"--network host",
		"--label watchman.managed=proxy",
		"--restart unless-stopped",
		proxyConfigDir + ":/etc/nginx/conf.d:ro",
		proxyCertDir + ":/opt/apps/nginx/certs:ro",
		"nginx:alpine",
		"docker logs --tail 30 " + nginxContainer,
	} {
		if !strings.Contains(run, want) {
			t.Errorf("run script must contain %q:\n%s", want, run)
		}
	}
	// Host networking binds 80/443 directly; a -p mapping would double-bind
	// or drift out of sync with the host network decision.
	if strings.Contains(run, " -p ") {
		t.Errorf("run script must not publish ports under --network host:\n%s", run)
	}
	// The reserved name must stay out of the user app namespace.
	if strings.HasPrefix(nginxContainer, "watchman-app-") {
		t.Errorf("proxy container %q must not live in the user app namespace", nginxContainer)
	}
}

// TestProxyContainerHealthy: only running + both mounts present counts as
// healthy; stopped, mis-mounted or hand-recreated containers trigger a rebuild.
func TestProxyContainerHealthy(t *testing.T) {
	healthy := "running|" + proxyConfigDir + ":/etc/nginx/conf.d;" + proxyCertDir + ":/opt/apps/nginx/certs;"
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"healthy", healthy, true},
		{"stopped", strings.Replace(healthy, "running", "exited", 1), false},
		{"restarting after crash loop", strings.Replace(healthy, "running", "restarting", 1), false},
		{"conf mount missing", "running|" + proxyCertDir + ":/opt/apps/nginx/certs;", false},
		{"cert mount missing", "running|" + proxyConfigDir + ":/etc/nginx/conf.d;", false},
		{"no separator", "running", false},
		{"empty", "", false},
		{"wrong mount target", "running|/opt/apps/nginx/conf.d:/etc/nginx/sites-enabled;", false},
	}
	for _, tc := range cases {
		if got := proxyContainerHealthy(tc.out); got != tc.want {
			t.Errorf("%s: proxyContainerHealthy(%q) = %v, want %v", tc.name, tc.out, got, tc.want)
		}
	}
}

// TestBindProxyRejectsWindowsHost: the ensure scripts assume a Linux host
// (docker run --network host, /opt paths); a Windows nginx host must get an
// explicit 400 before any docker exec or ACME issuance is attempted.
func TestBindProxyRejectsWindowsHost(t *testing.T) {
	store := newTestStore(t)
	// Register a Windows agent so both Hub() and GetAgent() resolve — the
	// bind must stop at the OS guard, never reach the ensure exec. The app's
	// HostID is the agent id (same as production data), known only after
	// registration, so the app is stored after.
	reg := rpc.NewRegistry(t.TempDir(), nil)
	token := reg.IssueEnrollToken()
	_, regResp, err := reg.Register(context.Background(), &agentpb.RegisterRequest{
		EnrollToken: token,
		Os:          "windows",
		Hostname:    "winbox",
	}, "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store.PutApp(&Application{
		ID: "app_win", Name: "demo", HostID: regResp.GetAgentId(),
		Ports: []PortMapping{{Host: 8080, Container: 80}},
	}); err != nil {
		t.Fatalf("PutApp: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewProxyHandler(reg, store, nil).Register(r.Group(""))
	req := httptest.NewRequest(http.MethodPut, "/apps/app_win/proxy",
		strings.NewReader(`{"domain":"x.example.com","mode":"local"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bind on Windows host: code = %d, want 400, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Linux") {
		t.Errorf("error must state the Linux-only limitation, body %s", w.Body.String())
	}
}
