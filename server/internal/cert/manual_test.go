package cert

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDNSACME is a minimal ACME server offering dns-01 for two identifiers
// ("example.com" and "*.example.com"). Triggering a challenge marks it valid
// (failChallenges makes it invalid instead), like a CA that found — or did
// not find — the TXT records. There is no real DNS involved.
type fakeDNSACME struct {
	t              *testing.T
	srv            *httptest.Server
	certPEM        []byte
	failChallenges bool

	mu    sync.Mutex
	valid [2]bool
	state [2]string // "pending" | "valid" | "invalid"
}

func newFakeDNSACME(t *testing.T, certPEM []byte, failChallenges bool) *fakeDNSACME {
	t.Helper()
	f := &fakeDNSACME{t: t, certPEM: certPEM, failChallenges: failChallenges}
	f.state = [2]string{"pending", "pending"}
	mux := http.NewServeMux()
	base := ""
	url := func(p string) string { return base + p }
	identValues := []string{"example.com", "*.example.com"}
	tokens := []string{"dns-token-1", "dns-token-2"}

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
		fmt.Fprintf(w, `{"status":"pending","authorizations":[%q,%q],"finalize":%q}`,
			url("/authz/1"), url("/authz/2"), url("/finalize/1"))
	})
	authzHandler := func(idx int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Replay-Nonce", "fake-nonce")
			f.mu.Lock()
			st := f.state[idx]
			f.mu.Unlock()
			chal := fmt.Sprintf(`{"type":"dns-01","url":%q,"token":%q,"status":%q}`,
				url(fmt.Sprintf("/chal/%d", idx+1)), tokens[idx], st)
			if st == "invalid" {
				chal = fmt.Sprintf(`{"type":"dns-01","url":%q,"token":%q,"status":"invalid",`+
					`"error":{"type":"urn:ietf:params:acme:error:dns","detail":"no TXT record found"}}`,
					url(fmt.Sprintf("/chal/%d", idx+1)), tokens[idx])
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":%q,"identifier":{"type":"dns","value":%q},"challenges":[%s]}`,
				st, identValues[idx], chal)
		}
	}
	mux.HandleFunc("/authz/1", authzHandler(0))
	mux.HandleFunc("/authz/2", authzHandler(1))
	chalHandler := func(idx int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Replay-Nonce", "fake-nonce")
			io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
			f.mu.Lock()
			if f.failChallenges {
				f.state[idx] = "invalid"
			} else {
				f.state[idx] = "valid"
				f.valid[idx] = true
			}
			st := f.state[idx]
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"type":"dns-01","status":%q,"token":%q}`, st, tokens[idx])
		}
	}
	mux.HandleFunc("/chal/1", chalHandler(0))
	mux.HandleFunc("/chal/2", chalHandler(1))
	mux.HandleFunc("/order/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fake-nonce")
		f.mu.Lock()
		ready := f.valid[0] && f.valid[1]
		f.mu.Unlock()
		status := "pending"
		if ready {
			status = "ready"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":%q,"finalize":%q,"certificate":%q}`,
			status, url("/finalize/1"), url("/cert/1"))
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

func (f *fakeDNSACME) close() { f.srv.Close() }

func newManualTestHub(t *testing.T, fake *fakeDNSACME) (*Hub, string) {
	t.Helper()
	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{
		Name:         "fake-dns-ca",
		DirectoryURL: fake.srv.URL + "/directory",
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}
	return h, dir
}

// stubDNSLookup replaces the real TXT pre-check (the fake CA has no DNS).
func stubDNSLookup(t *testing.T) {
	t.Helper()
	old := dnsTXTLookup
	dnsTXTLookup = func(host, want string) error { return nil }
	t.Cleanup(func() { dnsTXTLookup = old })
}

func TestDNS01RecordHost(t *testing.T) {
	cases := map[string]string{
		"example.com":       "_acme-challenge.example.com",
		"*.example.com":     "_acme-challenge.example.com",
		"sub.example.com":   "_acme-challenge.sub.example.com",
		"*.sub.example.com": "_acme-challenge.sub.example.com",
	}
	for in, want := range cases {
		if got := dns01RecordHost(in); got != want {
			t.Errorf("dns01RecordHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDNS01TXTValue(t *testing.T) {
	// RFC 8555 §8.4: digest = base64url(sha256(keyAuthorization)), no padding.
	keyAuthz := "test-token.thumbprint"
	sum := sha256.Sum256([]byte(keyAuthz))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	got := dns01TXTValue(keyAuthz)
	if got != want {
		t.Fatalf("dns01TXTValue = %q, want %q", got, want)
	}
	if len(got) != 43 {
		t.Fatalf("dns01TXTValue length = %d, want 43", len(got))
	}
	for _, r := range got {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			t.Fatalf("dns01TXTValue contains non-base64url char %q", r)
		}
	}
}

func TestStartManualDNSOrderRejectsIP(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Rejected before any network access: no account configured either.
	if _, err := h.StartManualDNSOrder([]string{"1.2.3.4"}, ""); err == nil {
		t.Fatal("expected IP identifier to be rejected for manual DNS-01")
	} else if !strings.Contains(err.Error(), "HTTP-01") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := h.StartManualDNSOrder(nil, ""); err == nil {
		t.Fatal("expected empty identifier list to be rejected")
	}
}

func TestValidPendingID(t *testing.T) {
	for _, bad := range []string{"", "mord_", "mord_zzz", "../x", "mord_" + strings.Repeat("g", 16),
		"mord_" + strings.Repeat("a", 15), "mord_" + strings.Repeat("a", 17), "crt_1234567890abcdef"} {
		if validPendingID(bad) {
			t.Errorf("validPendingID(%q) = true, want false", bad)
		}
	}
	if !validPendingID("mord_0123456789abcdef") {
		t.Error("validPendingID(valid) = false, want true")
	}
}

// TestPendingOrderPersistenceRoundTrip verifies internal ACME URLs/tokens
// survive a hub restart on disk, while the REST JSON shape never exposes them.
func TestPendingOrderPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &PendingOrder{
		ID: "mord_0123456789abcdef", AccountID: "acc_1",
		Identifiers: []string{"example.com"},
		Records:     []DNSRecord{{Domain: "example.com", Host: "_acme-challenge.example.com", Type: "TXT", Value: "v"}},
		OrderURL:    "https://ca.example/order/1",
		FinalizeURL: "https://ca.example/finalize/1",
		Authz: []pendingAuthz{{
			URL: "https://ca.example/authz/1", IdentifierType: "dns",
			IdentifierValue: "example.com", ChallengeURL: "https://ca.example/chal/1",
			Token: "secret-token",
		}},
		Status:    ManualOrderAwaitingDNS,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	h.mu.Lock()
	if err := h.savePendingLocked(p); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()

	// Reopen: internal fields must be intact.
	h2, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h2.GetPendingOrder(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OrderURL != p.OrderURL || got.FinalizeURL != p.FinalizeURL {
		t.Fatalf("order URLs lost after reload: %+v", got)
	}
	if len(got.Authz) != 1 || got.Authz[0].Token != "secret-token" || got.Authz[0].ChallengeURL == "" {
		t.Fatalf("authz lost after reload: %+v", got.Authz)
	}

	// REST shape: no internal URLs or tokens.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"ca.example", "secret-token", "order_url", "finalize_url", "challenge_url"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("REST JSON leaks internal field %q: %s", leak, b)
		}
	}
}

// TestManualDNSOrderEndToEnd runs the full two-phase flow against the fake
// dns-01 CA: start returns TXT records, confirm validates and stores the cert.
func TestManualDNSOrderEndToEnd(t *testing.T) {
	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeDNSACME(t, certPEM, false)
	defer fake.close()
	stubDNSLookup(t)

	h, dataDir := newManualTestHub(t, fake)
	order, err := h.StartManualDNSOrder([]string{"example.com", "*.example.com"}, "")
	if err != nil {
		t.Fatalf("StartManualDNSOrder: %v", err)
	}
	if order.Status != ManualOrderAwaitingDNS {
		t.Fatalf("status = %q, want awaiting_dns", order.Status)
	}
	if len(order.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(order.Records))
	}
	for _, r := range order.Records {
		if r.Host != "_acme-challenge.example.com" {
			t.Errorf("record host = %q, want _acme-challenge.example.com", r.Host)
		}
		if r.Type != "TXT" || len(r.Value) != 43 {
			t.Errorf("bad record: %+v", r)
		}
	}
	if order.Records[0].Value == order.Records[1].Value {
		t.Error("wildcard and apex records must carry distinct TXT values")
	}

	c, err := h.ConfirmManualDNSOrder(order.ID)
	if err != nil {
		t.Fatalf("ConfirmManualDNSOrder: %v", err)
	}
	if len(c.Domains) != 2 {
		t.Fatalf("cert domains = %v, want 2", c.Domains)
	}
	if c.AutoRenew {
		t.Error("manually issued certificate must not auto-renew; expiry is covered by the alert reminder")
	}
	if _, ok := h.Get(c.ID); !ok {
		t.Fatal("issued certificate missing from hub")
	}
	got, err := h.GetPendingOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ManualOrderDone || got.CertID != c.ID {
		t.Fatalf("pending order not marked done: %+v", got)
	}

	// Persistence: still done after hub reopen.
	h2, err := NewHub(dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := h2.GetPendingOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != ManualOrderDone || got2.CertID != c.ID {
		t.Fatalf("pending order state lost after reopen: %+v", got2)
	}
}

// TestManualDNSOrderConfirmFailsWhenInvalid pins the failure path: the CA
// rejects the challenges, the order lands in error state with the CA detail.
func TestManualDNSOrderConfirmFailsWhenInvalid(t *testing.T) {
	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeDNSACME(t, certPEM, true)
	defer fake.close()
	stubDNSLookup(t)

	h, _ := newManualTestHub(t, fake)
	order, err := h.StartManualDNSOrder([]string{"example.com"}, "")
	if err != nil {
		t.Fatalf("StartManualDNSOrder: %v", err)
	}
	if _, err := h.ConfirmManualDNSOrder(order.ID); err == nil {
		t.Fatal("expected confirm to fail when the CA rejects the challenges")
	}
	got, err := h.GetPendingOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ManualOrderError {
		t.Fatalf("status = %q, want error", got.Status)
	}
	if got.Error == "" {
		t.Fatal("error detail must be recorded on the pending order")
	}
}

// TestCancelManualDNSOrder verifies cancellation of a pending order:
// the on-disk state is removed, unknown IDs fail, and a verifying order
// is refused (the confirm path owns it until it settles).
func TestCancelManualDNSOrder(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &PendingOrder{
		ID: "mord_0123456789abcdef", AccountID: "acc_1",
		Identifiers: []string{"example.com"},
		Records:     []DNSRecord{{Domain: "example.com", Host: "_acme-challenge.example.com", Type: "TXT", Value: "v"}},
		Status:      ManualOrderAwaitingDNS,
		CreatedAt:   time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	h.mu.Lock()
	if err := h.savePendingLocked(p); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()

	if err := h.CancelManualDNSOrder(p.ID); err != nil {
		t.Fatalf("cancel awaiting order: %v", err)
	}
	if _, err := h.GetPendingOrder(p.ID); err == nil {
		t.Fatal("cancelled order should be gone")
	}

	// Unknown IDs are rejected.
	if err := h.CancelManualDNSOrder("mord_ffffffffffffffff"); err == nil {
		t.Fatal("expected error cancelling unknown order")
	}
	if err := h.CancelManualDNSOrder("not-an-id"); err == nil {
		t.Fatal("expected error cancelling malformed id")
	}

	// A verifying order cannot be cancelled mid-flight.
	p.Status = ManualOrderVerifying
	h.mu.Lock()
	if err := h.savePendingLocked(p); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()
	if err := h.CancelManualDNSOrder(p.ID); err == nil {
		t.Fatal("expected error cancelling a verifying order")
	} else if !strings.Contains(err.Error(), "验证中") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestManualDNSOrderTerminalError verifies the retry semantics: a CA terminal
// verdict (authorization invalid) marks the order terminal and a second
// confirm is rejected — the administrator must start a new order.
func TestManualDNSOrderTerminalError(t *testing.T) {
	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeDNSACME(t, certPEM, true) // CA marks challenges invalid
	defer fake.close()
	stubDNSLookup(t)

	h, _ := newManualTestHub(t, fake)
	order, err := h.StartManualDNSOrder([]string{"example.com"}, "")
	if err != nil {
		t.Fatalf("StartManualDNSOrder: %v", err)
	}
	if _, err := h.ConfirmManualDNSOrder(order.ID); err == nil {
		t.Fatal("expected confirm to fail when the CA rejects the challenges")
	}
	got, err := h.GetPendingOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Terminal {
		t.Fatal("CA invalid verdict must mark the order terminal")
	}
	if _, err := h.ConfirmManualDNSOrder(order.ID); err == nil {
		t.Fatal("expected re-confirm of a terminal order to be rejected")
	} else if !strings.Contains(err.Error(), "终态") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestManualDNSOrderRetryableError verifies a local pre-check failure stays
// retryable: nothing was sent to the CA, so confirming again after fixing
// DNS succeeds without re-provisioning records.
func TestManualDNSOrderRetryableError(t *testing.T) {
	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeDNSACME(t, certPEM, false)
	defer fake.close()

	h, _ := newManualTestHub(t, fake)
	// First attempt: DNS pre-check fails.
	dnsTXTLookup = func(host, want string) error { return fmt.Errorf("no TXT yet") }
	order, err := h.StartManualDNSOrder([]string{"example.com"}, "")
	if err != nil {
		t.Fatalf("StartManualDNSOrder: %v", err)
	}
	if _, err := h.ConfirmManualDNSOrder(order.ID); err == nil {
		t.Fatal("expected confirm to fail on DNS pre-check")
	}
	got, err := h.GetPendingOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Terminal {
		t.Fatal("pre-check failure must not be terminal")
	}
	if !strings.Contains(got.Error, "尚未通知 CA") {
		t.Fatalf("pre-check error should say the CA was not contacted: %q", got.Error)
	}
	// Administrator adds the TXT records; retry succeeds.
	stubDNSLookup(t)
	c, err := h.ConfirmManualDNSOrder(order.ID)
	if err != nil {
		t.Fatalf("retry confirm: %v", err)
	}
	if c == nil {
		t.Fatal("expected certificate on retry")
	}
	got, err = h.GetPendingOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ManualOrderDone || got.CertID == "" {
		t.Fatalf("status = %q cert = %q, want done", got.Status, got.CertID)
	}
}

// TestRecoverInterruptedPending verifies a restart never leaves an order
// stuck in "verifying": it flips to a retryable error instead.
func TestRecoverInterruptedPending(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &PendingOrder{
		ID: "mord_0123456789abcdef", AccountID: "acc_1",
		Identifiers: []string{"example.com"},
		Records:     []DNSRecord{{Domain: "example.com", Host: "_acme-challenge.example.com", Type: "TXT", Value: "v"}},
		Status:      ManualOrderVerifying,
		CreatedAt:   time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	h.mu.Lock()
	if err := h.savePendingLocked(p); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()

	// Simulate a restart.
	h2, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h2.GetPendingOrder(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ManualOrderError {
		t.Fatalf("status after restart = %q, want error", got.Status)
	}
	if got.Terminal {
		t.Fatal("interrupted verification must stay retryable")
	}
}
