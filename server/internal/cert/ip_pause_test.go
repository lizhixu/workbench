package cert

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// The orphan predicate is deliberately narrow: only bare-IP SAN lists
// qualify, so domain certificates (which apps may share) are never touched by
// the pause machinery.
func TestIsIPOnly(t *testing.T) {
	cases := []struct {
		name    string
		domains []string
		want    bool
	}{
		{"ipv4 only", []string{"203.0.113.10"}, true},
		{"ipv6 only", []string{"2001:db8::1"}, true},
		{"mixed ip and domain", []string{"203.0.113.10", "a.example.com"}, false},
		{"domain only", []string{"a.example.com"}, false},
		{"wildcard", []string{"*.example.com"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		if got := (&Certificate{Domains: tc.domains}).IsIPOnly(); got != tc.want {
			t.Errorf("%s: IsIPOnly = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Surrounding spaces are tolerated like everywhere else the SAN list is
	// parsed.
	if !(&Certificate{Domains: []string{" 203.0.113.10 "}}).IsIPOnly() {
		t.Error("padded IP SAN not recognized")
	}
}

func seedIssued(t *testing.T, hub *Hub, id string, domains []string, autoRenew bool) {
	t.Helper()
	now := time.Now()
	if err := hub.storeResult(id, "", domains, []byte("fake-cert"), []byte("fake-key"),
		now.Add(-24*time.Hour), now.Add(time.Hour), "test", autoRenew); err != nil {
		t.Fatalf("storeResult(%s): %v", id, err)
	}
}

// PauseOrphanedIPCerts flips auto-renew off for every issued IP-only
// certificate once the panel binds a domain, persists the flip (a restarted
// hub reads the same state), writes the reason into the certificate's log so
// the cert modal can explain it, and leaves domain certificates, already
// paused rows and placeholders untouched. Idempotent on repeat scans.
func TestPauseOrphanedIPCerts(t *testing.T) {
	dir := t.TempDir()
	hub, err := NewHub(dir, nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	seedIssued(t, hub, "crt_ip", []string{"203.0.113.10"}, true)
	seedIssued(t, hub, "crt_ip_paused", []string{"203.0.113.11"}, false)
	seedIssued(t, hub, "crt_domain", []string{"panel.example.com"}, true)
	if err := hub.createPendingCert("crt_ip_pending", "acc_test1", []string{"203.0.113.12"}, ""); err != nil {
		t.Fatalf("createPendingCert: %v", err)
	}
	hub.SetPanelDomainProvider(func() string { return "panel.example.com" })

	hub.PauseOrphanedIPCerts()

	if c, _ := hub.Get("crt_ip"); c == nil || c.AutoRenew {
		t.Fatalf("orphaned IP cert auto_renew still on: %+v", c)
	}
	if c, _ := hub.Get("crt_domain"); c == nil || !c.AutoRenew {
		t.Fatalf("domain cert auto_renew must not be touched: %+v", c)
	}
	if c, _ := hub.Get("crt_ip_pending"); c == nil || c.Status != CertStatusPending {
		t.Fatalf("pending placeholder must stay untouched: %+v", c)
	}
	note, err := os.ReadFile(hub.certLogFile("crt_ip"))
	if err != nil || !strings.Contains(string(note), "自动续期已自动暂停") {
		t.Fatalf("pause note missing from cert log (err=%v): %q", err, note)
	}
	if _, err := os.Stat(hub.certLogFile("crt_ip_paused")); err == nil {
		t.Error("already-paused row must not receive a pause note")
	}

	// Repeat scan is a no-op: no second note is appended.
	hub.PauseOrphanedIPCerts()
	note2, _ := os.ReadFile(hub.certLogFile("crt_ip"))
	if got := strings.Count(string(note2), "自动续期已自动暂停"); got != 1 {
		t.Errorf("pause note written %d times on repeat scans, want 1", got)
	}

	// The flip must survive a restart (saveCertsLocked persisted it).
	reloaded, err := NewHub(dir, nil)
	if err != nil {
		t.Fatalf("reload NewHub: %v", err)
	}
	if c, ok := reloaded.Get("crt_ip"); !ok || c.AutoRenew {
		t.Fatalf("pause did not persist across reload: %+v", c)
	}
	if c, ok := reloaded.Get("crt_domain"); !ok || !c.AutoRenew {
		t.Fatalf("domain cert changed across reload: %+v", c)
	}
}

// Without a bound panel domain (nil provider, or an empty domain) IP
// certificates keep renewing — that is the panel's own cert while the panel
// is reachable by IP.
func TestPauseOrphanedIPCertsNoopWithoutDomain(t *testing.T) {
	hub, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	seedIssued(t, hub, "crt_ip", []string{"203.0.113.10"}, true)

	hub.PauseOrphanedIPCerts() // nil provider
	hub.SetPanelDomainProvider(func() string { return "" })
	hub.PauseOrphanedIPCerts()

	if c, _ := hub.Get("crt_ip"); c == nil || !c.AutoRenew {
		t.Fatalf("IP cert paused without a bound domain: %+v", c)
	}
}

// renewExpiring reconciles orphans before filtering, so a bound domain stops
// every IP certificate from ever renewing again — starting with the very
// scan after the change. The domain certificate expiring in the same window
// is still attempted (and fails fast at account lookup), proving the loop
// itself ran.
func TestRenewExpiringSkipsOrphanedIP(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	hub, err := NewHub(t.TempDir(), log)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if err := hub.DeleteAccount("acc_letsencrypt"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	seedIssued(t, hub, "crt_ip", []string{"203.0.113.10"}, true)
	seedIssued(t, hub, "crt_domain", []string{"panel.example.com"}, true)
	hub.SetPanelDomainProvider(func() string { return "panel.example.com" })

	hub.renewExpiring()

	if !strings.Contains(logs.String(), "orphaned IP certificate auto-renew paused") {
		t.Error("renewExpiring did not reconcile orphaned IP certificates first")
	}
	if strings.Contains(logs.String(), `msg="renewing certificate" id=crt_ip`) {
		t.Error("renewExpiring selected the orphaned IP certificate for renewal")
	}
	if !strings.Contains(logs.String(), `msg="renewing certificate" id=crt_domain`) {
		t.Error("expiring domain certificate was not attempted; the loop did not run")
	}
	if c, _ := hub.Get("crt_ip"); c == nil || c.AutoRenew || c.LastError != "" {
		t.Errorf("orphaned IP cert mutated beyond the pause: %+v", c)
	}
	if c, _ := hub.Get("crt_domain"); c == nil || c.LastError == "" {
		t.Error("domain cert was not actually attempted (last_error empty)")
	}
}

// Manual renewal of an orphaned IP certificate must be refused with an
// actionable message instead of burning CA quota for material nobody can
// serve.
func TestRenewRefusesOrphanedIP(t *testing.T) {
	hub, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	seedIssued(t, hub, "crt_ip", []string{"203.0.113.10"}, true)
	hub.SetPanelDomainProvider(func() string { return "panel.example.com" })

	if _, err := hub.Renew("crt_ip"); err == nil || !strings.Contains(err.Error(), "IP 证书不再适用") {
		t.Fatalf("Renew(orphaned IP) err = %v, want IP 证书不再适用", err)
	}
	c, _ := hub.Get("crt_ip")
	if c == nil || c.Renewing || c.LastError != "" {
		t.Fatalf("refused Renew mutated the row: %+v", c)
	}
}

// Without a bound domain the same IP certificate stays renewable: the Renew
// passes the guards and fails at account lookup (accounts removed), proving
// the refusal above is the only thing standing in the way.
func TestRenewAllowsIPWithoutBoundDomain(t *testing.T) {
	hub, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if err := hub.DeleteAccount("acc_letsencrypt"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	seedIssued(t, hub, "crt_ip", []string{"203.0.113.10"}, true)

	if _, err := hub.Renew("crt_ip"); err == nil || strings.Contains(err.Error(), "IP 证书不再适用") {
		t.Fatalf("Renew(IP, no domain) err = %v, want a non-guard failure (no default account)", err)
	}
	if c, _ := hub.Get("crt_ip"); c == nil || c.LastError == "" {
		t.Fatal("Renew did not proceed past the guards (last_error empty)")
	}
}

// Reissuing for bare IPs while a domain is bound is refused up front, but a
// record whose identifiers are edited to hostnames stays re-issuable — the
// guard matches the requested identifiers, not the stored record.
func TestStartReissueTaskRefusesIPIdentifiers(t *testing.T) {
	hub, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if err := hub.DeleteAccount("acc_letsencrypt"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	seedIssued(t, hub, "crt_ip", []string{"203.0.113.10"}, true)
	hub.SetPanelDomainProvider(func() string { return "panel.example.com" })

	if _, _, err := hub.StartReissueTask("crt_ip", []string{"203.0.113.10"}, "", ChallengeHTTP01); err == nil || !strings.Contains(err.Error(), "IP 证书不再适用") {
		t.Fatalf("StartReissueTask(IP identifiers) err = %v, want IP 证书不再适用", err)
	}
	// Hostname identifiers on the same record are a legitimate conversion;
	// the flow must not be refused by the IP guard (it fails later at
	// account lookup, which is the next step).
	if _, _, err := hub.StartReissueTask("crt_ip", []string{"panel.example.com"}, "", ChallengeHTTP01); err == nil || strings.Contains(err.Error(), "IP 证书不再适用") {
		t.Fatalf("StartReissueTask(hostname identifiers) err = %v, want a non-guard failure", err)
	}
}
