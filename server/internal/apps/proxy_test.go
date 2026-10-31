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
	"watchman/server/internal/network"
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

// TestAppsForCertPush: renewal re-push target selection — identity first:
// an app binding whose CertID matches the renewed cert is always re-pushed.
// Legacy entries (empty CertID) fall back to deterministic FindByDomain.
// Target host is resolved per proxy mode: local → the app host, gateway → the recorded gateway host.
func TestAppsForCertPush(t *testing.T) {
	crt := &cert.Certificate{ID: "crt_target", Domains: []string{"api.example.com", "*.shop.org"}}
	apps := []*Application{
		{ID: "a1", Name: "local", HostID: "h1", ProxyDomains: []DomainBinding{{Domain: "api.example.com", CertID: "crt_target"}}, ProxyMode: "local"},
		{ID: "a2", Name: "gateway", HostID: "h2", ProxyDomains: []DomainBinding{{Domain: "www.shop.org", CertID: "crt_target"}}, ProxyMode: "gateway", ProxyGatewayHostID: "gw1"},
		{ID: "a3", Name: "unbound", HostID: "h3", ProxyDomains: nil, ProxyMode: "local"},
		{ID: "a4", Name: "unrelated", HostID: "h4", ProxyDomains: []DomainBinding{{Domain: "other.org", CertID: "crt_other"}}, ProxyMode: "local"},
		{ID: "a5", Name: "wildcard-other-cert", HostID: "h5", ProxyDomains: []DomainBinding{{Domain: "shop.org", CertID: "crt_other"}}, ProxyMode: "local"},
		{ID: "a6", Name: "gateway-no-host", HostID: "h6", ProxyDomains: []DomainBinding{{Domain: "api.example.com", CertID: "crt_target"}}, ProxyMode: "gateway", ProxyGatewayHostID: ""},
	}
	ph := NewProxyHandler(nil, nil, nil)
	got := ph.appsForCertPush(crt, apps)
	if len(got) != 3 {
		t.Fatalf("selected %d apps, want 3: %+v", len(got), got)
	}
	byID := map[string]repushTarget{}
	for _, tr := range got {
		byID[tr.app.ID] = tr
	}
	if byID["a1"].targetHost != "h1" {
		t.Errorf("local mode must target the app host, got %q", byID["a1"].targetHost)
	}
	if byID["a2"].targetHost != "gw1" {
		t.Errorf("gateway mode must target the gateway host, got %q", byID["a2"].targetHost)
	}
	if len(byID["a2"].bindings) != 1 || byID["a2"].bindings[0].Domain != "www.shop.org" {
		t.Errorf("domain must be normalized as bound, got %+v", byID["a2"].bindings)
	}
	if byID["a6"].targetHost != "h6" {
		t.Errorf("gateway without a recorded host falls back to the app host, got %q", byID["a6"].targetHost)
	}
	for _, id := range []string{"a3", "a4", "a5"} {
		if _, ok := byID[id]; ok {
			t.Errorf("app %s must not be selected (empty/unrelated/different cert)", id)
		}
	}
	// A nil certificate selects nothing.
	if ph.appsForCertPush(nil, apps) != nil {
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
		ID:                  "app_gw",
		Name:                "demo",
		HostID:              "h1",
		ProxyDomains:        []DomainBinding{{Domain: "www.example.com", CertID: "crt1"}},
		ProxyMode:           "gateway",
		ProxyUpstream:       "127.0.0.1:8080",
		ProxyUpstreamManual: true,
		ProxyGatewayHostID:  "gw1",
		Ports:               []PortMapping{{Host: 8080, Container: 80}},
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
	if len(got.ProxyDomains) != 0 || got.ProxyMode != "" || got.ProxyUpstream != "" || got.ProxyGatewayHostID != "" || got.ProxyUpstreamManual {
		t.Fatalf("unbind must clear the whole binding incl. gateway host and manual flag: %+v", got)
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

		hub, err := cert.NewHub(t.TempDir(), nil)
		if err != nil {
			t.Fatalf("NewHub: %v", err)
		}
		_ = hub.StoreCertForTest("c1", []string{"x.example.com"}, "mock-cert", "mock-key")

		gin.SetMode(gin.TestMode)
		r := gin.New()
		NewProxyHandler(reg, store, hub).Register(r.Group(""))
		req := httptest.NewRequest(http.MethodPut, "/apps/app_win/proxy",
			strings.NewReader(`{"domains":[{"domain":"x.example.com","cert_id":"c1"}],"mode":"local"}`))
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

// TestBindProxyValidation: bind auto-resolves an omitted cert_id to the best
// usable covering certificate, and rejects uncovered domains (with the whole
// missing list), non-existent certs, non-covering certs, certs without key
// material, and duplicate domains in the request — always before touching any
// host, so a rejected request must leave the stored binding untouched.
func TestBindProxyValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newTestStore(t)
	hub, err := cert.NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	_ = hub.StoreCertForTest("crt_example", []string{"example.com", "*.example.com"}, "mock-cert", "mock-key")
	_ = hub.StoreCertForTest("crt_nokey", []string{"nokey.com"}, "mock-cert", "")

	app := &Application{
		ID:     "app1",
		Name:   "app-one",
		HostID: "h1",
		Ports:  []PortMapping{{Host: 8080, Container: 80}},
	}
	if err := store.PutApp(app); err != nil {
		t.Fatalf("PutApp: %v", err)
	}

	reg := rpc.NewRegistry(t.TempDir(), nil)
	ph := NewProxyHandler(reg, store, hub)
	r := gin.New()
	ph.Register(r.Group(""))

	cases := []struct {
		name       string
		body       string
		wantCode   int
		wantSubstr string
	}{
		{
			name:       "empty domains",
			body:       `{"domains":[]}`,
			wantCode:   http.StatusBadRequest,
			wantSubstr: "至少绑定一个域名",
		},
		{
			name:       "auto with no covering cert lists missing domains",
			body:       `{"domains":[{"domain":"uncovered.org","cert_id":""}]}`,
			wantCode:   http.StatusBadRequest,
			wantSubstr: `"missing_domains":["uncovered.org"]`,
		},
		{
			name:       "auto skips a keyless covering cert",
			body:       `{"domains":[{"domain":"nokey.com","cert_id":""}]}`,
			wantCode:   http.StatusBadRequest,
			wantSubstr: `"missing_domains":["nokey.com"]`,
		},
		{
			name:       "duplicate domain in request",
			body:       `{"domains":[{"domain":"sub.example.com","cert_id":"crt_example"},{"domain":"sub.example.com","cert_id":"crt_example"}]}`,
			wantCode:   http.StatusBadRequest,
			wantSubstr: "在请求中重复",
		},
		{
			name:       "cert does not exist",
			body:       `{"domains":[{"domain":"sub.example.com","cert_id":"nonexistent"}]}`,
			wantCode:   http.StatusBadRequest,
			wantSubstr: "指定的证书不存在",
		},
		{
			name:       "cert does not cover domain",
			body:       `{"domains":[{"domain":"other.org","cert_id":"crt_example"}]}`,
			wantCode:   http.StatusBadRequest,
			wantSubstr: "所选证书不覆盖域名",
		},
		{
			name:       "cert missing key material",
			body:       `{"domains":[{"domain":"nokey.com","cert_id":"crt_nokey"}]}`,
			wantCode:   http.StatusConflict,
			wantSubstr: "证书材料缺失",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/apps/app1/proxy", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d, body = %s", w.Code, tc.wantCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantSubstr) {
				t.Fatalf("body %s does not contain %q", w.Body.String(), tc.wantSubstr)
			}
		})
	}
	// Every case is rejected before any host write, so nothing may land on the
	// stored app — a partially persisted binding would point nginx at a vhost
	// whose certificate files were never pushed.
	if got, ok := store.GetApp("app1"); !ok {
		t.Fatalf("GetApp: app not found")
	} else if len(got.ProxyDomains) != 0 || got.ProxyMode != "" || got.ProxyUpstream != "" {
		t.Errorf("rejected bind must persist nothing, got domains=%+v mode=%q upstream=%q",
			got.ProxyDomains, got.ProxyMode, got.ProxyUpstream)
	}
}

// TestBindProxyCrossAppConflict: binding a domain already bound by another app
// must return 409 Conflict.
func TestBindProxyCrossAppConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newTestStore(t)
	_ = store.PutApp(&Application{
		ID:           "app_existing",
		Name:         "ExistingApp",
		HostID:       "h1",
		ProxyDomains: []DomainBinding{{Domain: "clash.example.com", CertID: "crt1"}},
	})
	_ = store.PutApp(&Application{
		ID:     "app_new",
		Name:   "NewApp",
		HostID: "h1",
	})
	reg := rpc.NewRegistry(t.TempDir(), nil)
	hub, err := cert.NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	_ = hub.StoreCertForTest("crt1", []string{"clash.example.com"}, "mock-cert", "mock-key")

	ph := NewProxyHandler(reg, store, hub)
	r := gin.New()
	ph.Register(r.Group(""))

	req := httptest.NewRequest(http.MethodPut, "/apps/app_new/proxy",
		strings.NewReader(`{"domains":[{"domain":"clash.example.com","cert_id":"crt1"}],"mode":"local"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ExistingApp") {
		t.Fatalf("expected conflict message mentioning ExistingApp, got %s", w.Body.String())
	}
}

// TestRenderVhosts: renderVhosts generates an 80 redirect and 443 proxy server
// block for each domain.
func TestRenderVhosts(t *testing.T) {
	bindings := []DomainBinding{
		{Domain: "a.example.com", CertID: "c1"},
		{Domain: "b.example.com", CertID: "c2"},
	}
	conf := renderVhosts(bindings, "127.0.0.1:8080", true)
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if !strings.Contains(conf, "server_name "+d+";") {
			t.Errorf("conf must declare server_name %s:\n%s", d, conf)
		}
		if !strings.Contains(conf, proxyCertDir+"/"+d+".crt;") {
			t.Errorf("conf must reference cert for %s:\n%s", d, conf)
		}
		if !strings.Contains(conf, proxyCertDir+"/"+d+".key;") {
			t.Errorf("conf must reference key for %s:\n%s", d, conf)
		}
	}
	if !strings.Contains(conf, "proxy_pass http://127.0.0.1:8080;") {
		t.Errorf("conf must proxy_pass to upstream:\n%s", conf)
	}
	if !strings.Contains(conf, "Upgrade $http_upgrade;") {
		t.Errorf("websocket enabled must include Upgrade header:\n%s", conf)
	}
}

// TestValidProxyUpstream: a manual upstream is interpolated verbatim into a
// root-written nginx config, so only IP/hostname:port survives — anything
// carrying shell/nginx metacharacters must be rejected before render.
func TestValidProxyUpstream(t *testing.T) {
	valid := []string{
		"127.0.0.1:8080",
		"100.64.0.5:9000",
		"1.2.3.4:1",
		"1.2.3.4:65535",
		"app.internal:8080",
		"[::1]:8080",
	}
	for _, u := range valid {
		if !validProxyUpstream(u) {
			t.Errorf("validProxyUpstream(%q) = false, want true", u)
		}
	}
	invalid := []string{
		"",
		"127.0.0.1",
		"127.0.0.1:",
		":8080",
		"127.0.0.1:0",
		"127.0.0.1:65536",
		"127.0.0.1:abc",
		"127.0.0.1:+8080",
		"127.0.0.1:+80",
		"127.0.0.1:8080; rm -rf /",
		"127.0.0.1:8080}",
		"127.0.0.1:8080\nserver_name evil",
		"ho st:8080",
		"example.com:8080#foo",
		"example.com:8080$var",
		"a:b:c:8080",
		"-lead.example.com:8080",
		"double..dot.com:8080",
	}
	for _, u := range invalid {
		if validProxyUpstream(u) {
			t.Errorf("validProxyUpstream(%q) = true, want false", u)
		}
	}
}

// TestGatewayStoredUpstreamWasComputed: the sync-time rule that tells a
// computed gateway upstream (still naming the previous app host) apart from
// a manual override that must survive a migration.
func TestGatewayStoredUpstreamWasComputed(t *testing.T) {
	cases := []struct {
		name                            string
		stored                          string
		oldMesh, oldPublic, oldInternal string
		firstPort                       int
		want                            bool
	}{
		{"computed mesh ip", "100.64.0.5:8080", "100.64.0.5", "1.2.3.4", "10.0.0.5", 8080, true},
		{"computed public ip", "1.2.3.4:8080", "100.64.0.5", "1.2.3.4", "10.0.0.5", 8080, true},
		{"computed internal ip", "10.0.0.5:8080", "100.64.0.5", "1.2.3.4", "10.0.0.5", 8080, true},
		{"manual port differs from published", "100.64.0.5:9000", "100.64.0.5", "1.2.3.4", "10.0.0.5", 8080, false},
		{"manual host unrelated to old host", "203.0.113.9:8080", "100.64.0.5", "1.2.3.4", "10.0.0.5", 8080, false},
		{"old host offline yields no match", "100.64.0.5:8080", "", "", "", 8080, false},
		{"empty stored", "", "100.64.0.5", "1.2.3.4", "10.0.0.5", 8080, false},
		{"unparseable stored", "not-an-upstream", "100.64.0.5", "1.2.3.4", "10.0.0.5", 8080, false},
	}
	for _, tc := range cases {
		got := gatewayStoredUpstreamWasComputed(tc.stored, tc.oldMesh, tc.oldPublic, tc.oldInternal, tc.firstPort)
		if got != tc.want {
			t.Errorf("%s: gatewayStoredUpstreamWasComputed(%q) = %v, want %v", tc.name, tc.stored, got, tc.want)
		}
	}
}

// TestSyncUpstreamPreservesManual: syncUpstream keeps a stored upstream
// (local always; gateway when it is not the old host's computed value) and
// falls back to defaultUpstream only for empty/malformed stored values.
func TestSyncUpstreamPreservesManual(t *testing.T) {
	ph := NewProxyHandler(nil, nil, nil)
	local := &Application{ID: "a1", HostID: "h1", ProxyUpstream: "127.0.0.1:9000", Ports: []PortMapping{{Host: 8080, Container: 80}}}
	got, err := ph.syncUpstream(local, "local", "", "h0")
	if err != nil || got != "127.0.0.1:9000" {
		t.Errorf("local manual upstream: got %q, err %v, want 127.0.0.1:9000", got, err)
	}
	gw := &Application{ID: "a2", HostID: "h2", ProxyUpstream: "203.0.113.9:8080", Ports: []PortMapping{{Host: 8080, Container: 80}}}
	got, err = ph.syncUpstream(gw, "gateway", "gw1", "h0")
	if err != nil || got != "203.0.113.9:8080" {
		t.Errorf("gateway manual upstream: got %q, err %v, want 203.0.113.9:8080", got, err)
	}
	empty := &Application{ID: "a3", HostID: "h3", Ports: []PortMapping{{Host: 8080, Container: 80}}}
	got, err = ph.syncUpstream(empty, "local", "", "h0")
	if err != nil || got != "127.0.0.1:8080" {
		t.Errorf("local empty upstream: got %q, err %v, want 127.0.0.1:8080", got, err)
	}
}

// TestSyncUpstreamManualFlag: a gateway upstream flagged manual survives
// even when it names the old host on the first published port — the exact
// collision the old-host heuristic would otherwise recompute away. The
// old host's mesh IP is seeded through the network store so the heuristic
// would fire for an unflagged record; the unflagged twin must instead
// attempt recomputation (here it errors: nothing else resolves).
func TestSyncUpstreamManualFlag(t *testing.T) {
	ns, err := network.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("network store: %v", err)
	}
	if err := ns.SetNodeStatus(network.NodeStatus{HostID: "h0", IP: "100.64.0.5"}); err != nil {
		t.Fatalf("SetNodeStatus: %v", err)
	}
	reg := rpc.NewRegistry(t.TempDir(), nil)
	ph := NewProxyHandler(reg, nil, nil)
	ph.SetNetworkStore(ns)
	manual := &Application{ID: "a1", HostID: "h2", ProxyUpstream: "100.64.0.5:8080", ProxyUpstreamManual: true, Ports: []PortMapping{{Host: 8080, Container: 80}}}
	got, err := ph.syncUpstream(manual, "gateway", "gw1", "h0")
	if err != nil || got != "100.64.0.5:8080" {
		t.Errorf("manual gateway upstream: got %q, err %v, want 100.64.0.5:8080", got, err)
	}
	legacy := &Application{ID: "a2", HostID: "h2", ProxyUpstream: "100.64.0.5:8080", Ports: []PortMapping{{Host: 8080, Container: 80}}}
	if got, err := ph.syncUpstream(legacy, "gateway", "gw1", "h0"); err == nil {
		t.Errorf("legacy gateway upstream naming old host must recompute, got %q without error", got)
	}
}

// TestReloadFailureImplicatesApp: compensation after a failed nginx -t must
// only fire when the output names this app's conf or cert files — another
// app's broken conf must not delete this app's good binding.
func TestReloadFailureImplicatesApp(t *testing.T) {
	conf := "/opt/apps/nginx/conf.d/watchman-app1.conf"
	cases := []struct {
		name   string
		output string
		pushed []string
		want   bool
	}{
		{"own conf full path", "nginx: [emerg] unexpected end in /opt/apps/nginx/conf.d/watchman-app1.conf:12", nil, true},
		{"own conf basename", "nginx: [emerg] directive error in watchman-app1.conf:3", nil, true},
		{"own cert missing", "nginx: [emerg] cannot load certificate \"/opt/apps/nginx/certs/a.example.com.crt\"", []string{"a.example.com"}, true},
		{"other app conf", "nginx: [emerg] unexpected end in /opt/apps/nginx/conf.d/watchman-app2.conf:12", []string{"a.example.com"}, false},
		{"other app cert", "nginx: [emerg] cannot load certificate \"/opt/apps/nginx/certs/b.example.com.crt\"", []string{"a.example.com"}, false},
		{"generic no file", "nginx: [emerg] unknown directive \"foo\"", []string{"a.example.com"}, false},
		{"empty output", "", []string{"a.example.com"}, false},
	}
	for _, tc := range cases {
		if got := reloadFailureImplicatesApp(tc.output, conf, tc.pushed); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRenderVhostsWebsocketFlag: the stored ProxyWebSocket choice (not a
// hardcoded true) decides whether Upgrade/Connection headers render — a
// migration must not silently flip websocket support on.
func TestRenderVhostsWebsocketFlag(t *testing.T) {
	bindings := []DomainBinding{{Domain: "a.example.com", CertID: "c1"}}
	off := renderVhosts(bindings, "127.0.0.1:8080", false)
	if strings.Contains(off, "Upgrade $http_upgrade;") {
		t.Errorf("websocket=false must not render Upgrade header:\n%s", off)
	}
	on := renderVhosts(bindings, "127.0.0.1:8080", true)
	if !strings.Contains(on, "Upgrade $http_upgrade;") {
		t.Errorf("websocket=true must render Upgrade header:\n%s", on)
	}
}

// TestProxyUnbindScriptQuotesPaths: stored domains predate strict bind-time
// validation, so the unbind rm command must quote every path — a legacy
// dirty domain must never break out of the command.
func TestProxyUnbindScriptQuotesPaths(t *testing.T) {
	app := &Application{ID: "app1", ProxyDomains: []DomainBinding{{Domain: "a.example.com", CertID: "c1"}}}
	script := proxyUnbindScript(app, "/opt/apps/nginx/conf.d/watchman-app1.conf")
	for _, want := range []string{
		"'/opt/apps/nginx/conf.d/watchman-app1.conf'",
		"'/opt/apps/nginx/certs/a.example.com.crt'",
		"'/opt/apps/nginx/certs/a.example.com.key'",
		"docker exec watchman-proxy-nginx nginx -s reload",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("unbind script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "|| true") {
		t.Errorf("unbind script must not mask the reload result with || true:\n%s", script)
	}
}

// TestResolveProxyCerts: an empty cert_id means auto — the binding is
// materialized with the concrete ID of the best usable covering certificate,
// while every uncovered/unusable domain is collected into one missing list so
// the caller can batch-issue them. Explicit IDs keep the strict
// existence/coverage/key checks.
func TestResolveProxyCerts(t *testing.T) {
	hub, err := cert.NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	_ = hub.StoreCertForTest("crt_a", []string{"a.example.com"}, "cert", "key")
	_ = hub.StoreCertForTest("crt_wild", []string{"*.example.com"}, "cert", "key")
	_ = hub.StoreCertForTest("crt_nokey", []string{"nokey.test"}, "cert", "")

	pairs, missing, rerr := resolveProxyCerts(hub, []DomainBinding{
		{Domain: "a.example.com"},
		{Domain: "sub.example.com"},
		{Domain: "nokey.test"},
		{Domain: "nothing.org"},
	})
	if rerr != nil {
		t.Fatalf("auto resolve: unexpected error %v", rerr)
	}
	if len(missing) != 2 || missing[0] != "nokey.test" || missing[1] != "nothing.org" {
		t.Errorf("missing = %v, want [nokey.test nothing.org]", missing)
	}
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d, want 2", len(pairs))
	}
	if pairs[0].binding.CertID != "crt_a" || !pairs[0].binding.Auto {
		t.Errorf("exact SAN should materialize crt_a as auto, got %+v", pairs[0].binding)
	}
	if pairs[1].binding.CertID != "crt_wild" || pairs[1].crt.ID != "crt_wild" || !pairs[1].binding.Auto {
		t.Errorf("wildcard should materialize crt_wild as auto, got binding=%+v crt=%q", pairs[1].binding, pairs[1].crt.ID)
	}

	// Explicit override is honored even when auto would pick a different cert,
	// and it is recorded as a deliberate choice (auto=false).
	pairs, missing, rerr = resolveProxyCerts(hub, []DomainBinding{{Domain: "a.example.com", CertID: "crt_wild"}})
	if rerr != nil || len(missing) != 0 || len(pairs) != 1 || pairs[0].binding.CertID != "crt_wild" || pairs[0].binding.Auto {
		t.Fatalf("explicit override: pairs=%v missing=%v err=%v", pairs, missing, rerr)
	}

	// Explicit failures keep their old classification.
	cases := []struct {
		name       string
		binding    DomainBinding
		wantStatus int
		wantSubstr string
	}{
		{name: "missing cert", binding: DomainBinding{Domain: "a.example.com", CertID: "nope"}, wantStatus: http.StatusBadRequest, wantSubstr: "指定的证书不存在"},
		{name: "cert does not cover", binding: DomainBinding{Domain: "b.org", CertID: "crt_a"}, wantStatus: http.StatusBadRequest, wantSubstr: "不覆盖域名"},
		{name: "cert missing key", binding: DomainBinding{Domain: "nokey.test", CertID: "crt_nokey"}, wantStatus: http.StatusConflict, wantSubstr: "证书材料缺失"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, rerr := resolveProxyCerts(hub, []DomainBinding{tc.binding})
			if rerr == nil {
				t.Fatalf("expected error, got nil")
			}
			if rerr.status != tc.wantStatus || !strings.Contains(rerr.msg, tc.wantSubstr) {
				t.Errorf("got status=%d msg=%q, want status=%d containing %q", rerr.status, rerr.msg, tc.wantStatus, tc.wantSubstr)
			}
		})
	}
}
