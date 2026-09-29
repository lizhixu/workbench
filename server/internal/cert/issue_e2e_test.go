package cert

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeACME is a minimal ACME server for end-to-end issuance tests. It offers
// http-01 for the IP identifier 127.0.0.1, validates the challenge by
// fetching it from the solver's own listener (like a real CA would), then
// serves a self-signed certificate for download.
type fakeACME struct {
	t          *testing.T
	srv        *httptest.Server
	token      string
	solverAddr string // host:port of the HTTP-01 listener under test

	mu         sync.Mutex
	authzValid bool
	certPEM    []byte
}

func newFakeACME(t *testing.T, certPEM []byte, solverAddr string) *fakeACME {
	t.Helper()
	f := &fakeACME{t: t, token: "e2e-test-token", certPEM: certPEM, solverAddr: solverAddr}
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
		fmt.Fprintf(w, `{"status":%q,"identifier":{"type":"ip","value":"127.0.0.1"},`+
			`"challenges":[{"type":"http-01","url":%q,"token":%q}]}`,
			status, url("/chal/1"), f.token)
	})
	// Challenge trigger: validate like a real CA by fetching the response
	// from the solver's listener.
	mux.HandleFunc("/chal/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
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
