package cert

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Renew must refuse a pending placeholder: an issuance task is already
// running for it (StartIssueTask holds acmeMu), and renewing here would
// queue a second full issuance for the same domains.
func TestRenewRefusesPending(t *testing.T) {
	hub, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if err := hub.createPendingCert("crt_pending1", "acc_test1", []string{"test.example.com"}, ""); err != nil {
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

// A failed placeholder must stay renewable: that is how an operator retries
// a failed issuance. With every ACME account removed, Renew passes the
// in-lock guards and then fails fast at account lookup (no network); the
// resulting non-guard error and the rewritten LastError (markError only runs
// after the guards) prove the retry was not refused.
func TestRenewAllowsFailedRetry(t *testing.T) {
	hub, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if err := hub.DeleteAccount("acc_letsencrypt"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if err := hub.createPendingCert("crt_failed1", "acc_test1", []string{"retry.example.com"}, ""); err != nil {
		t.Fatalf("createPendingCert: %v", err)
	}
	hub.markIssueFailed("crt_failed1", "DNS-01 验证超时")
	if c, _ := hub.Get("crt_failed1"); c.Status != CertStatusFailed {
		t.Fatalf("setup: status = %q, want failed", c.Status)
	}
	if _, err := hub.Renew("crt_failed1"); err == nil {
		t.Fatalf("Renew(failed) unexpectedly succeeded with no ACME account")
	} else if strings.Contains(err.Error(), "正在签发中") || strings.Contains(err.Error(), "正在续期中") {
		t.Fatalf("Renew(failed) was refused by a guard, retry is broken: %v", err)
	}
	c, _ := hub.Get("crt_failed1")
	if c.LastError == "DNS-01 验证超时" || c.LastError == "" {
		t.Fatalf("Renew(failed) did not proceed past the guards: last_error=%q", c.LastError)
	}
}

// renewExpiring must never pick up pending or failed placeholders. Both
// carry AutoRenew=true with a zero NotAfter, so without the status guard
// time.Until(zero) qualifies them on every 6h tick. A slog tap is the
// independent evidence: the loop logs "renewing certificate" with the row id
// BEFORE calling Renew, so a placeholder id appearing in the log means the
// skip was lost. The old status/LastError assertion alone was a false
// positive, because Renew's own pending guard also leaves those untouched.
func TestRenewExpiringSkipsPlaceholders(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	hub, err := NewHub(t.TempDir(), log)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	// Remove the auto-seeded account so any Renew attempt fails fast at
	// account lookup instead of reaching the ACME network.
	if err := hub.DeleteAccount("acc_letsencrypt"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	now := time.Now()
	if err := hub.createPendingCert("crt_pending1", "acc_test1", []string{"pending.example.com"}, ""); err != nil {
		t.Fatalf("createPendingCert: %v", err)
	}
	if err := hub.createPendingCert("crt_failed1", "acc_test1", []string{"failed.example.com"}, ""); err != nil {
		t.Fatalf("createPendingCert: %v", err)
	}
	hub.markIssueFailed("crt_failed1", "DNS-01 验证超时")
	// Genuinely issued, expiring within the hour: the renewal attempt must
	// happen (and fail fast at account lookup), proving the loop itself ran.
	if err := hub.storeResult("crt_issued1", "", []string{"issued.example.com"}, []byte("fake-cert"), []byte("fake-key"), now.Add(-24*time.Hour), now.Add(time.Hour), "test", true); err != nil {
		t.Fatalf("storeResult: %v", err)
	}

	hub.renewExpiring()

	for _, id := range []string{"crt_pending1", "crt_failed1"} {
		if strings.Contains(logs.String(), id) {
			t.Errorf("renewExpiring selected placeholder %s for renewal (skip lost)", id)
		}
	}
	if !strings.Contains(logs.String(), "crt_issued1") {
		t.Errorf("issued expiring cert was not attempted by renewExpiring")
	}
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
	if err := hub.createPendingCert("crt_pending1", "acc_test1", []string{"pending.example.com"}, ""); err != nil {
		t.Fatalf("createPendingCert: %v", err)
	}
	if err := hub.Delete("crt_pending1"); !errors.Is(err, ErrCertificateBeingIssued) {
		t.Fatalf("Delete(pending) err = %v, want ErrCertificateBeingIssued", err)
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

// deleteCert must map a pending delete to 409 (temporary conflict) rather
// than 404. Panel/app binding guards are nil here, so the status comes
// solely from the pending row; the hub-level ErrCertificateBeingIssued
// backstop is asserted in TestDeleteRefusesPending.
func TestDeleteCertPendingReturnsConflict(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.createPendingCert("crt_pending1", "acc_test1", []string{"pending.example.com"}, ""); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandlers(h, nil, nil).Register(r.Group(""), r.Group(""))
	req := httptest.NewRequest(http.MethodDelete, "/certs/crt_pending1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete(pending) status = %d, want 409 (body %s)", w.Code, w.Body.String())
	}
	if _, ok := h.Get("crt_pending1"); !ok {
		t.Fatal("pending cert vanished despite 409")
	}
}
