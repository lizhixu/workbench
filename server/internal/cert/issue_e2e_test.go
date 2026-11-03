package cert

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeACME is a minimal ACME server for end-to-end issuance tests. By
// default it offers http-01 for the IP identifier 127.0.0.1, validates the
// challenge by fetching it from the solver's own listener (like a real CA
// would), then serves a self-signed certificate for download. Tests may
// reconfigure the identifier/challenge shape or make the CA reject the
// challenge outright.
type fakeACME struct {
	t          *testing.T
	srv        *httptest.Server
	token      string
	solverAddr string // host:port of the HTTP-01 listener under test

	// Identifier and challenge shape; the defaults keep the original IP +
	// http-01 behavior the success-path tests rely on. The handlers read
	// these fields at request time, so tests may override after construction.
	idType   string
	idValue  string
	chalType string

	// failChallenge makes the CA reject the challenge trigger outright,
	// driving the CA-side failure path (challengeFailedError).
	failChallenge bool

	mu         sync.Mutex
	authzValid bool
	orders     int
	certPEM    []byte
}

func newFakeACME(t *testing.T, certPEM []byte, solverAddr string) *fakeACME {
	t.Helper()
	f := &fakeACME{t: t, token: "e2e-test-token", certPEM: certPEM, solverAddr: solverAddr,
		idType: "ip", idValue: "127.0.0.1", chalType: "http-01"}
	mux := http.NewServeMux()
	base := ""
	url := func(p string) string { return base + p }

	mux.HandleFunc("/directory", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"newNonce":%q,"newAccount":%q,"newOrder":%q}`,
			url("/new-nonce"), url("/new-account"), url("/new-order"))
	})
	mux.HandleFunc("/new-nonce", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/new-account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", url("/account/1"))
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"status":"valid"}`)
	})
	mux.HandleFunc("/new-order", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.orders++
		f.mu.Unlock()
		w.Header().Set("Location", url("/order/1"))
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"status":"pending","authorizations":[%q],"finalize":%q}`,
			url("/authz/1"), url("/finalize/1"))
	})
	mux.HandleFunc("/authz/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		f.mu.Lock()
		valid := f.authzValid
		f.mu.Unlock()
		status := "pending"
		if valid {
			status = "valid"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":%q,"identifier":{"type":%q,"value":%q},`+
			`"challenges":[{"type":%q,"url":%q,"token":%q}]}`,
			status, f.idType, f.idValue, f.chalType, url("/chal/1"), f.token)
	})
	// Challenge trigger: validate like a real CA by fetching the response
	// from the solver's listener — or reject it outright when configured.
	mux.HandleFunc("/chal/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		if f.failChallenge {
			http.Error(w, "challenge rejected by CA (test)", http.StatusBadRequest)
			return
		}
		checkURL := fmt.Sprintf("http://%s/.well-known/acme-challenge/%s", f.solverAddr, f.token)
		resp, err := http.Get(checkURL)
		if err != nil {
			http.Error(w, "challenge fetch failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), f.token+".") {
			http.Error(w, "challenge response mismatch", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.authzValid = true
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"http-01","status":"valid","token":"`+f.token+`"}`)
	})
	mux.HandleFunc("/order/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ready","finalize":%q,"certificate":%q}`,
			url("/finalize/1"), url("/cert/1"))
	})
	mux.HandleFunc("/finalize/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"valid","certificate":%q}`, url("/cert/1"))
	})
	mux.HandleFunc("/cert/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		w.Write(f.certPEM)
	})
	f.srv = httptest.NewServer(mux)
	base = f.srv.URL
	return f
}

func (f *fakeACME) close() { f.srv.Close() }

// TestIssueStoresInListEndToEnd runs a full Hub.Issue against the fake CA
// using HTTP-01 (no dns-mng configured) and verifies the issued certificate
// lands in the hub's issued list — the path the panel one-click issuance and
// the cert-center issuance both rely on.
func TestIssueStoresInListEndToEnd(t *testing.T) {
	// Reserve a loopback port for the HTTP-01 listener under test.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve test port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	solverAddr := "127.0.0.1:" + strconv.Itoa(port)

	oldAddr := http01ListenAddr
	http01ListenAddr = solverAddr
	defer func() { http01ListenAddr = oldAddr }()

	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeACME(t, certPEM, solverAddr)
	defer fake.close()

	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// dns-mng intentionally left unconfigured: HTTP-01 alone must suffice.
	if err := h.PutAccount(&ACMEAccount{
		Name:         "fake-ca",
		DirectoryURL: fake.srv.URL + "/directory",
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}

	c, err := h.Issue([]string{"127.0.0.1"}, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(c.Domains) != 1 || c.Domains[0] != "127.0.0.1" {
		t.Fatalf("Domains = %v, want [127.0.0.1]", c.Domains)
	}

	found := false
	for _, e := range h.List() {
		if e.ID == c.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("issued certificate missing from List()")
	}
	if got, ok := h.Get(c.ID); !ok || got.CertPEM == "" || got.KeyPEM == "" {
		t.Fatal("Get() did not return stored PEM material")
	}

	// Persistence: still listed after reopening the hub.
	h2, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h2.Get(c.ID); !ok {
		t.Fatal("issued certificate missing after hub reopen")
	}
}

// TestStartIssueTaskStoresRecordEndToEnd covers the async task path used by
// the cert-center UI ("申请 SSL 证书" button): StartIssueTask must mark the
// task success with the new cert ID and the certificate must land in the
// issued list — the record the list page shows after creation completes.
func TestStartIssueTaskStoresRecordEndToEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve test port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	solverAddr := "127.0.0.1:" + strconv.Itoa(port)

	oldAddr := http01ListenAddr
	http01ListenAddr = solverAddr
	defer func() { http01ListenAddr = oldAddr }()

	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeACME(t, certPEM, solverAddr)
	defer fake.close()

	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{
		Name:         "fake-ca",
		DirectoryURL: fake.srv.URL + "/directory",
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}

	task, err := h.StartIssueTask([]string{"127.0.0.1"}, "", ChallengeAuto)
	if err != nil {
		t.Fatalf("StartIssueTask: %v", err)
	}
	if task.ID == "" || task.Status != TaskRunning {
		t.Fatalf("task = %+v, want running task with ID", task)
	}

	// Poll the task to its terminal state, like the UI log modal does.
	deadline := time.Now().Add(30 * time.Second)
	var final *IssueTask
	for time.Now().Before(deadline) {
		got, ok := h.GetTask(task.ID)
		if !ok {
			t.Fatal("task disappeared from GetTask")
		}
		if got.Status != TaskRunning {
			final = got
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("issue task did not finish in time")
	}
	if final.Status != TaskSuccess {
		t.Fatalf("task status = %s, error = %q, log = %s", final.Status, final.Error, final.Log)
	}
	if final.CertID == "" {
		t.Fatal("successful task must record the issued cert ID")
	}

	// The record must be visible in the issued list right after the task
	// reaches success — no manual reload should be needed for it to exist.
	cert, ok := h.Get(final.CertID)
	if !ok {
		t.Fatalf("certificate %s missing from hub storage after task success", final.CertID)
	}
	if len(cert.Domains) != 1 || cert.Domains[0] != "127.0.0.1" {
		t.Fatalf("Domains = %v, want [127.0.0.1]", cert.Domains)
	}
	if cert.CertPEM == "" || cert.KeyPEM == "" {
		t.Fatal("stored certificate is missing PEM material")
	}
	found := false
	for _, e := range h.List() {
		if e.ID == final.CertID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("issued certificate missing from List()")
	}
}

// TestIssueHTTP01ViaSharedResponder reproduces the domain-bound panel
// deployment: the panel's own standard-port listener owns :80, so the issuance
// flow's temporary bind fails and the CA's challenge fetch is answered by the
// panel's HTTP stack through the shared registry (ServeSharedHTTP01). HTTP-01
// must stay usable end to end — this is the path the cert-center "申请 SSL
// 证书" flow and the panel one-click certificate take on such deployments.
func TestIssueHTTP01ViaSharedResponder(t *testing.T) {
	// Occupy the would-be temporary listener port so hc.start() fails, like
	// it does when the panel's own listener holds :80.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve test port: %v", err)
	}
	defer blocker.Close()
	oldAddr := http01ListenAddr
	http01ListenAddr = blocker.Addr().String()
	defer func() { http01ListenAddr = oldAddr }()

	certPEM, _ := makeTestCertPEM(t)

	// The panel's own :80 stand-in: challenge requests are answered from the
	// shared registry, everything else 404s like the panel would.
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ServeSharedHTTP01(w, r) {
			http.NotFound(w, r)
		}
	}))
	defer panel.Close()
	panelAddr := panel.Listener.Addr().String()

	// The CA fetches the challenge from the panel listener, not from a
	// solver-owned one.
	fake := newFakeACME(t, certPEM, panelAddr)
	defer fake.close()

	SetSharedHTTP01Probe(func() bool { return true })
	defer SetSharedHTTP01Probe(nil)

	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{
		Name:         "fake-ca",
		DirectoryURL: fake.srv.URL + "/directory",
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}

	c, err := h.Issue([]string{"127.0.0.1"}, "")
	if err != nil {
		t.Fatalf("Issue via shared responder: %v", err)
	}
	found := false
	for _, e := range h.List() {
		if e.ID == c.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("issued certificate missing from List()")
	}
}

// TestIssueWithChallengePreferencePinsMechanism verifies that an explicit
// challenge preference restricts issuance to that mechanism and fails fast
// when it is unusable (no silent fallback when the user pinned a method).
func TestIssueWithChallengePreferencePinsMechanism(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{
		Name:         "dummy",
		DirectoryURL: "https://example.com/directory",
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}
	// dns-mng intentionally unconfigured: pinning DNS-01 must fail before
	// any network activity.
	if _, err := h.IssueWithChallenge([]string{"example.com"}, "", ChallengeDNS01); err == nil ||
		!strings.Contains(err.Error(), "dns-mng") {
		t.Fatalf("want dns-mng error, got %v", err)
	}
	if _, err := h.IssueWithChallenge([]string{"example.com"}, "", ChallengePreference("bogus")); err == nil {
		t.Fatal("bogus preference should fail")
	}
}

// TestIssuePinnedDNS01SurfacesRealFailure pins DNS-01 explicitly (the issuance
// dialog's "DNS-01（dns-mng）" option) and makes the CA reject the challenge.
// A pinned preference leaves no other mechanism to fall back to, so the flow
// must surface the real CA-side failure and stop after exactly one order —
// re-ordering with an empty solver set would burn a fresh CA order and bury
// the cause under a "no usable challenge" error. The automatic mode keeps its
// cross-mechanism fallback and is covered separately by the success tests.
func TestIssuePinnedDNS01SurfacesRealFailure(t *testing.T) {
	// Fake dns-mng: accepts TXT present/cleanup calls.
	dnsMng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/acme/dns01/present", "/api/acme/dns01/cleanup":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer dnsMng.Close()

		// Stub propagation check and min wait for fast test execution.
		oldCheck := dns01AuthoritativeCheck
		dns01AuthoritativeCheck = func(ctx context.Context, fqdn, want string) error { return nil }
		defer func() { dns01AuthoritativeCheck = oldCheck }()
		oldMinWait := dns01PropagationMinWait
		dns01PropagationMinWait = 0
		defer func() { dns01PropagationMinWait = oldMinWait }()

	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeACME(t, certPEM, "")
	fake.idType, fake.idValue, fake.chalType = "dns", "e2e.example.com", "dns-01"
	fake.failChallenge = true
	defer fake.close()

	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.UpdateConfig(Config{Enabled: true, BaseURL: dnsMng.URL}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{
		Name:         "fake-ca",
		DirectoryURL: fake.srv.URL + "/directory",
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = h.IssueWithChallenge([]string{"e2e.example.com"}, "", ChallengeDNS01)
	if err == nil {
		t.Fatal("issuance must fail when the CA rejects the challenge")
	}
	if strings.Contains(err.Error(), "no usable challenge") {
		t.Fatalf("error must surface the real CA-side failure, got the empty-fallback artifact: %v", err)
	}
	if !strings.Contains(err.Error(), "dns-01") || !strings.Contains(err.Error(), "验证") {
		t.Fatalf("error should identify the failed DNS-01 validation: %v", err)
	}
		fake.mu.Lock()
		orders := fake.orders
		fake.mu.Unlock()
		if orders != 1 {
			t.Fatalf("created %d CA orders, want exactly 1 (no doomed fallback order after a pinned mechanism fails)", orders)
		}
	}

type fakeBatchAuthz struct {
	idType     string
	idValue    string
	wildcard   bool
	chalType   string
	token      string
	authzValid bool
}

type fakeBatchACME struct {
	t             *testing.T
	srv           *httptest.Server
	authzs        []*fakeBatchAuthz
	failChallenge bool
	orders        int
	certPEM       []byte
	mu            sync.Mutex
	record        func(string)
}

func newFakeBatchACME(t *testing.T, certPEM []byte, authzs []*fakeBatchAuthz, record func(string)) *fakeBatchACME {
	t.Helper()
	f := &fakeBatchACME{
		t:       t,
		certPEM: certPEM,
		authzs:  authzs,
		record:  record,
	}
	mux := http.NewServeMux()
	base := ""
	url := func(p string) string { return base + p }

	mux.HandleFunc("/directory", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"newNonce":%q,"newAccount":%q,"newOrder":%q}`,
			url("/new-nonce"), url("/new-account"), url("/new-order"))
	})
	mux.HandleFunc("/new-nonce", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/new-account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", url("/account/1"))
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"status":"valid"}`)
	})
	mux.HandleFunc("/new-order", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.orders++
		f.mu.Unlock()
		w.Header().Set("Location", url("/order/1"))
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.WriteHeader(http.StatusCreated)
		var authzURLs []string
		for i := range f.authzs {
			authzURLs = append(authzURLs, fmt.Sprintf("%q", url(fmt.Sprintf("/authz/%d", i))))
		}
		fmt.Fprintf(w, `{"status":"pending","authorizations":[%s],"finalize":%q}`,
			strings.Join(authzURLs, ","), url("/finalize/1"))
	})
	mux.HandleFunc("/authz/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		idxStr := strings.TrimPrefix(r.URL.Path, "/authz/")
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 || idx >= len(f.authzs) {
			http.NotFound(w, r)
			return
		}
		f.record(fmt.Sprintf("ca:poll:authz/%d", idx))
		f.mu.Lock()
		az := f.authzs[idx]
		valid := az.authzValid
		f.mu.Unlock()
		status := "pending"
		if valid {
			status = "valid"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":%q,"identifier":{"type":%q,"value":%q},"wildcard":%v,`+
			`"challenges":[{"type":%q,"url":%q,"token":%q}]}`,
			status, az.idType, az.idValue, az.wildcard, az.chalType, url(fmt.Sprintf("/chal/%d", idx)), az.token)
	})
	mux.HandleFunc("/chal/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		idxStr := strings.TrimPrefix(r.URL.Path, "/chal/")
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 || idx >= len(f.authzs) {
			http.NotFound(w, r)
			return
		}
		f.record(fmt.Sprintf("ca:trigger:chal/%d", idx))
		if f.failChallenge {
			http.Error(w, "challenge rejected by CA (test)", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.authzs[idx].authzValid = true
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":%q,"status":"valid","token":%q}`, f.authzs[idx].chalType, f.authzs[idx].token)
	})
	mux.HandleFunc("/order/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ready","finalize":%q,"certificate":%q}`,
			url("/finalize/1"), url("/cert/1"))
	})
	mux.HandleFunc("/finalize/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		f.record("ca:finalize")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"valid","certificate":%q}`, url("/cert/1"))
	})
	mux.HandleFunc("/cert/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		f.record("ca:download")
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		w.Write(f.certPEM)
	})
	f.srv = httptest.NewServer(mux)
	base = f.srv.URL
	return f
}

func (f *fakeBatchACME) close() { f.srv.Close() }

// TestIssueBatchPresentDNS01EndToEnd verifies the Lego / RFC 8555 batch pattern:
// 1. All TXT records are presented in batch (both coexist in DNS provider).
// 2. Authoritative NS propagation is verified before contacting the CA.
// 3. CA triggers and polls are dispatched in batch after propagation check succeeds.
// 4. Deferred cleanup deletes temporary TXT records only after the order is finalized and downloaded.
func TestIssueBatchPresentDNS01EndToEnd(t *testing.T) {
	var events []string
	var eventsMu sync.Mutex
	recordEvent := func(ev string) {
		eventsMu.Lock()
		events = append(events, ev)
		eventsMu.Unlock()
	}

	dnsMng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		fqdn, _ := req["fqdn"].(string)
		val, _ := req["value"].(string)
		switch r.URL.Path {
		case "/api/acme/dns01/present":
			recordEvent(fmt.Sprintf("dns-mng:present:%s:%s", fqdn, val))
			w.WriteHeader(http.StatusOK)
		case "/api/acme/dns01/cleanup":
			recordEvent(fmt.Sprintf("dns-mng:cleanup:%s:%s", fqdn, val))
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer dnsMng.Close()

	oldCheck := dns01AuthoritativeCheck
	dns01AuthoritativeCheck = func(ctx context.Context, fqdn, want string) error {
		recordEvent(fmt.Sprintf("dns-check:%s:%s", fqdn, want))
		return nil
	}
	defer func() { dns01AuthoritativeCheck = oldCheck }()

	oldMinWait := dns01PropagationMinWait
	dns01PropagationMinWait = 0
	defer func() { dns01PropagationMinWait = oldMinWait }()

	certPEM, _ := makeTestCertPEM(t)
	// Two authorizations for single domain + wildcard: lizhixu.cn & *.lizhixu.cn
	fake := newFakeBatchACME(t, certPEM, []*fakeBatchAuthz{
		{idType: "dns", idValue: "lizhixu.cn", wildcard: false, chalType: "dns-01", token: "tok-single"},
		{idType: "dns", idValue: "lizhixu.cn", wildcard: true, chalType: "dns-01", token: "tok-wildcard"},
	}, recordEvent)
	defer fake.close()

	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.UpdateConfig(Config{Enabled: true, BaseURL: dnsMng.URL}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{
		Name:         "fake-batch-ca",
		DirectoryURL: fake.srv.URL + "/directory",
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}

	cert, err := h.IssueWithChallenge([]string{"lizhixu.cn", "*.lizhixu.cn"}, "", ChallengeDNS01)
	if err != nil {
		t.Fatalf("IssueWithChallenge failed: %v", err)
	}
	if cert == nil || cert.ID == "" {
		t.Fatal("expected issued certificate")
	}

	// Verify timeline ordering:
	// All dns-mng:present must occur BEFORE any ca:trigger
	// All dns-check must occur BEFORE any ca:trigger
	// ca:download must occur BEFORE any dns-mng:cleanup
	eventsMu.Lock()
	defer eventsMu.Unlock()

	var presentIndices []int
	var checkIndices []int
	var triggerIndices []int
	var downloadIndex = -1
	var cleanupIndices []int

	for i, ev := range events {
		switch {
		case strings.HasPrefix(ev, "dns-mng:present:"):
			presentIndices = append(presentIndices, i)
		case strings.HasPrefix(ev, "dns-check:"):
			checkIndices = append(checkIndices, i)
		case strings.HasPrefix(ev, "ca:trigger:"):
			triggerIndices = append(triggerIndices, i)
		case ev == "ca:download":
			downloadIndex = i
		case strings.HasPrefix(ev, "dns-mng:cleanup:"):
			cleanupIndices = append(cleanupIndices, i)
		}
	}

	if len(presentIndices) != 2 {
		t.Fatalf("expected 2 present calls, got %d: %v", len(presentIndices), events)
	}
	if len(checkIndices) != 2 {
		t.Fatalf("expected 2 dns propagation checks, got %d: %v", len(checkIndices), events)
	}
	if len(triggerIndices) != 2 {
		t.Fatalf("expected 2 ca triggers, got %d: %v", len(triggerIndices), events)
	}
	if downloadIndex == -1 {
		t.Fatalf("expected ca:download event in %v", events)
	}
	if len(cleanupIndices) != 2 {
		t.Fatalf("expected 2 cleanups, got %d: %v", len(cleanupIndices), events)
	}

	// 1. Both presents happen before the first trigger
	firstTrigger := triggerIndices[0]
	for _, pIdx := range presentIndices {
		if pIdx > firstTrigger {
			t.Errorf("present at index %d happened after first trigger at %d: %v", pIdx, firstTrigger, events)
		}
	}

	// 2. Both checks happen before the first trigger
	for _, cIdx := range checkIndices {
		if cIdx > firstTrigger {
			t.Errorf("check at index %d happened after first trigger at %d: %v", cIdx, firstTrigger, events)
		}
	}

	// 3. Deferred cleanup: cleanups happen after download
	for _, clIdx := range cleanupIndices {
		if clIdx < downloadIndex {
			t.Errorf("cleanup at index %d happened before download at %d (premature cleanup!): %v", clIdx, downloadIndex, events)
		}
	}
}

// TestIssueBatchPresentPropagationFailNoTrigger ensures that if authoritative propagation
// check fails, the CA is never triggered (no authz poisoning), cleanup is still executed,
// and no fallback reordering is attempted.
func TestIssueBatchPresentPropagationFailNoTrigger(t *testing.T) {
	var triggered bool
	var cleanedUp int
	var mu sync.Mutex

	dnsMng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/acme/dns01/cleanup" {
			mu.Lock()
			cleanedUp++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer dnsMng.Close()

	oldCheck := dns01AuthoritativeCheck
	dns01AuthoritativeCheck = func(ctx context.Context, fqdn, want string) error {
		return fmt.Errorf("simulated authoritative propagation timeout for %s", fqdn)
	}
	defer func() { dns01AuthoritativeCheck = oldCheck }()

	oldMinWait := dns01PropagationMinWait
	dns01PropagationMinWait = 0
	defer func() { dns01PropagationMinWait = oldMinWait }()

	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeBatchACME(t, certPEM, []*fakeBatchAuthz{
		{idType: "dns", idValue: "test.example.com", wildcard: false, chalType: "dns-01", token: "tok1"},
	}, func(ev string) {
		if strings.HasPrefix(ev, "ca:trigger:") {
			mu.Lock()
			triggered = true
			mu.Unlock()
		}
	})
	defer fake.close()

	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.UpdateConfig(Config{Enabled: true, BaseURL: dnsMng.URL}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{
		Name:         "fake-batch-ca",
		DirectoryURL: fake.srv.URL + "/directory",
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = h.IssueWithChallenge([]string{"test.example.com"}, "", ChallengeDNS01)
	if err == nil {
		t.Fatal("expected error on propagation failure")
	}
	if !strings.Contains(err.Error(), "权威服务器传播校验失败") {
		t.Fatalf("expected propagation error, got: %v", err)
	}

	mu.Lock()
	trig := triggered
	cl := cleanedUp
	mu.Unlock()

	if trig {
		t.Error("CA must NOT be triggered when propagation check fails")
	}
	if cl != 1 {
		t.Errorf("cleanup count = %d, want 1", cl)
	}

	fake.mu.Lock()
	orders := fake.orders
	fake.mu.Unlock()
	if orders != 1 {
		t.Fatalf("orders = %d, want 1 (propagation failure must not retry CA orders)", orders)
	}
}

// TestIssueRetryGuardWildcardDNSFailNoReorder verifies that for an order containing a wildcard,
// when DNS-01 validation fails at the CA, the issuance terminates immediately without attempting
// a doomed second order using HTTP-01.
func TestIssueRetryGuardWildcardDNSFailNoReorder(t *testing.T) {
	dnsMng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer dnsMng.Close()

	oldCheck := dns01AuthoritativeCheck
	dns01AuthoritativeCheck = func(ctx context.Context, fqdn, want string) error { return nil }
	defer func() { dns01AuthoritativeCheck = oldCheck }()

	oldMinWait := dns01PropagationMinWait
	dns01PropagationMinWait = 0
	defer func() { dns01PropagationMinWait = oldMinWait }()

	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeBatchACME(t, certPEM, []*fakeBatchAuthz{
		{idType: "dns", idValue: "example.com", wildcard: false, chalType: "dns-01", token: "tok-single"},
		{idType: "dns", idValue: "example.com", wildcard: true, chalType: "dns-01", token: "tok-wildcard"},
	}, func(string) {})
	fake.failChallenge = true
	defer fake.close()

	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.UpdateConfig(Config{Enabled: true, BaseURL: dnsMng.URL}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{
		Name:         "fake-batch-ca",
		DirectoryURL: fake.srv.URL + "/directory",
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}

	// ChallengeAuto: HTTP-01 might be available, but wildcard is in identifiers!
	_, err = h.IssueWithChallenge([]string{"example.com", "*.example.com"}, "", ChallengeAuto)
	if err == nil {
		t.Fatal("expected issuance failure")
	}
	if !strings.Contains(err.Error(), "dns-01") && !strings.Contains(err.Error(), "DNS-01") {
		t.Fatalf("error must identify DNS-01 failure: %v", err)
	}
	if strings.Contains(err.Error(), "no usable challenge") {
		t.Fatalf("error must not be empty fallback 'no usable challenge', got: %v", err)
	}

	fake.mu.Lock()
	orders := fake.orders
	fake.mu.Unlock()
	if orders != 1 {
		t.Fatalf("orders = %d, want exactly 1 (no doomed HTTP-01 fallback order when order has wildcard)", orders)
	}
}
