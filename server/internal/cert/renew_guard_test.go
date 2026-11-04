package cert

import (
	"strings"
	"testing"
	"time"
)

// Renew must refuse a pending placeholder: an issuance task is already
// running for it (StartIssueTask holds acmeMu), and renewing here would
// queue a second full issuance for the same domains.
func TestRenewRefusesPending(t *testing.T) {
	hub, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if err := hub.createPendingCert("crt_pending1", "acc_test1", []string{"test.example.com"}); err != nil {
		t.Fatalf("createPendingCert: %v", err)
	}
	if _, err := hub.Renew("crt_pending1"); err == nil || !strings.Contains(err.Error(), "正在签发中") {
		t.Fatalf("Renew(pending) err = %v, want '正在签发中' refusal", err)
	}
	c, _ := hub.Get("crt_pending1")
	if c.Status != CertStatusPending || c.Renewing {
		t.Errorf("pending cert mutated by refused Renew: status=%q renewing=%v", c.Status, c.Renewing)
	}
}

// renewExpiring must never pick up pending or failed placeholders. Both
// carry AutoRenew=true with a zero NotAfter, so without the status guard
// time.Until(zero) qualifies them on every 6h tick: pending would
// double-issue, failed would retry forever.
func TestRenewExpiringSkipsPlaceholders(t *testing.T) {
	hub, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	now := time.Now()
	if err := hub.createPendingCert("crt_pending1", "acc_test1", []string{"pending.example.com"}); err != nil {
		t.Fatalf("createPendingCert: %v", err)
	}
	if err := hub.createPendingCert("crt_failed1", "acc_test1", []string{"failed.example.com"}); err != nil {
		t.Fatalf("createPendingCert: %v", err)
	}
	hub.markIssueFailed("crt_failed1", "DNS-01 验证超时")
	// Genuinely issued, expiring within the hour, no ACME account: the
	// renewal attempt must happen and fail at account lookup, proving the
	// loop itself ran while the placeholders were skipped.
	if err := hub.storeResult("crt_issued1", "", []string{"issued.example.com"}, []byte("fake-cert"), []byte("fake-key"), now.Add(-24*time.Hour), now.Add(time.Hour), "test", true); err != nil {
		t.Fatalf("storeResult: %v", err)
	}

	hub.renewExpiring()

	if c, _ := hub.Get("crt_pending1"); c.Status != CertStatusPending || c.LastError != "" {
		t.Errorf("pending cert touched by renewExpiring: status=%q last_error=%q", c.Status, c.LastError)
	}
	if c, _ := hub.Get("crt_failed1"); c.Status != CertStatusFailed || c.LastError != "DNS-01 验证超时" {
		t.Errorf("failed cert retried by renewExpiring: status=%q last_error=%q", c.Status, c.LastError)
	}
	if c, _ := hub.Get("crt_issued1"); c.LastError == "" {
		t.Errorf("issued expiring cert was not attempted: last_error still empty")
	}
}

// Delete must refuse a pending placeholder: the in-flight task's
// storeResult would otherwise recreate the row as issued on success,
// silently undoing the operator's delete. Failed rows have no live task
// and stay deletable.
func TestDeleteRefusesPending(t *testing.T) {
	hub, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if err := hub.createPendingCert("crt_pending1", "acc_test1", []string{"pending.example.com"}); err != nil {
		t.Fatalf("createPendingCert: %v", err)
	}
	if err := hub.Delete("crt_pending1"); err == nil {
		t.Fatalf("Delete(pending) succeeded, want refusal")
	}
	if _, ok := hub.Get("crt_pending1"); !ok {
		t.Fatalf("pending cert vanished despite refused Delete")
	}
	hub.markIssueFailed("crt_pending1", "boom")
	if err := hub.Delete("crt_pending1"); err != nil {
		t.Fatalf("Delete(failed) = %v, want success", err)
	}
	if _, ok := hub.Get("crt_pending1"); ok {
		t.Fatalf("failed cert still present after Delete")
	}
}
