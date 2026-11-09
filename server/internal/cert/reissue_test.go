package cert

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func waitForTaskEnd(t *testing.T, h *Hub, taskID string) *IssueTask {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got, ok := h.GetTask(taskID)
		if !ok {
			t.Fatal("task disappeared from GetTask")
		}
		if got.Status != TaskRunning {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("task did not finish in time")
	return nil
}

func newReissueTestHub(t *testing.T, fakeCertPEM []byte) (*Hub, *fakeACME, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve test port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	solverAddr := "127.0.0.1:" + strconv.Itoa(port)

	oldAddr := http01ListenAddr
	http01ListenAddr = solverAddr
	t.Cleanup(func() { http01ListenAddr = oldAddr })

	fake := newFakeACME(t, fakeCertPEM, solverAddr)
	t.Cleanup(fake.close)

	h, err := NewHub(t.TempDir(), nil)
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
	def, err := h.GetDefaultAccount()
	if err != nil {
		t.Fatal(err)
	}
	return h, fake, def.ID
}

// TestStartReissueTaskEndToEnd: re-issuing an existing record under a new
// challenge stores the fresh material back into the same record (same ID),
// keeps the admin remark, persists the challenge preference, and preserves
// the auto-renew setting.
func TestStartReissueTaskEndToEnd(t *testing.T) {
	newCertPEM, _ := makeTestCertPEM(t)
	h, _, accountID := newReissueTestHub(t, newCertPEM)
	oldCertPEM, oldKeyPEM := makeTestCertPEM(t)
	now := time.Now()
	if err := h.storeResult("crt_old", accountID, []string{"127.0.0.1"}, oldCertPEM, oldKeyPEM, now, now.Add(30*24*time.Hour), "Old CA", true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.UpdateRemark("crt_old", "生产核心证书"); err != nil {
		t.Fatal(err)
	}

	task, _, err := h.StartReissueTask("crt_old", []string{"127.0.0.1"}, "", ChallengeHTTP01)
	if err != nil {
		t.Fatalf("StartReissueTask: %v", err)
	}
	if task.CertID != "crt_old" {
		t.Fatalf("task.CertID = %q, want the existing record id", task.CertID)
	}
	final := waitForTaskEnd(t, h, task.ID)
	if final.Status != TaskSuccess {
		t.Fatalf("task status = %s, error = %q, log = %s", final.Status, final.Error, final.Log)
	}

	got, ok := h.Get("crt_old")
	if !ok {
		t.Fatal("crt_old missing after reissue")
	}
	if got.Status != CertStatusIssued || got.Renewing {
		t.Fatalf("Status = %q Renewing = %v, want issued/not-renewing", got.Status, got.Renewing)
	}
	if got.Remark != "生产核心证书" {
		t.Fatalf("remark = %q, want preserved", got.Remark)
	}
	if got.Challenge != ChallengeHTTP01 {
		t.Fatalf("Challenge = %q, want http-01 persisted", got.Challenge)
	}
	if got.AccountID != accountID {
		t.Fatalf("AccountID = %q, want %q", got.AccountID, accountID)
	}
	if !got.AutoRenew {
		t.Fatal("auto-renew setting must be preserved by a reissue")
	}
	if len(got.Domains) != 1 || got.Domains[0] != "127.0.0.1" {
		t.Fatalf("Domains = %v", got.Domains)
	}
	if strings.TrimSpace(got.CertPEM) != strings.TrimSpace(string(newCertPEM)) {
		t.Fatal("stored material must be the freshly issued certificate, not the old one")
	}
}

// TestStartReissueTaskFailureKeepsOldMaterial: when the CA rejects the
// reissue, the record keeps serving the old material with its old domains.
func TestStartReissueTaskFailureKeepsOldMaterial(t *testing.T) {
	newCertPEM, _ := makeTestCertPEM(t)
	h, fake, accountID := newReissueTestHub(t, newCertPEM)
	fake.failChallenge = true
	oldCertPEM, oldKeyPEM := makeTestCertPEM(t)
	now := time.Now()
	if err := h.storeResult("crt_old", accountID, []string{"127.0.0.1"}, oldCertPEM, oldKeyPEM, now, now.Add(30*24*time.Hour), "Old CA", true); err != nil {
		t.Fatal(err)
	}

	task, _, err := h.StartReissueTask("crt_old", []string{"127.0.0.1"}, "", ChallengeHTTP01)
	if err != nil {
		t.Fatalf("StartReissueTask: %v", err)
	}
	final := waitForTaskEnd(t, h, task.ID)
	if final.Status != TaskFailed {
		t.Fatalf("task status = %s, want failed", final.Status)
	}

	got, ok := h.Get("crt_old")
	if !ok {
		t.Fatal("crt_old missing after failed reissue")
	}
	if got.Status != CertStatusIssued {
		t.Fatalf("Status = %q, want issued (a failed reissue must never downgrade the record)", got.Status)
	}
	if got.Renewing {
		t.Fatal("Renewing must be cleared after a failed reissue")
	}
	if got.CertPEM != string(oldCertPEM) || len(got.Domains) != 1 || got.Domains[0] != "127.0.0.1" {
		t.Fatal("failed reissue must not touch the stored material or domains")
	}
	if got.LastError == "" {
		t.Fatal("failed reissue must record LastError")
	}
}

func TestStartReissueTaskGuards(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{Name: "acc1", DirectoryURL: "https://example.invalid/directory", IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := makeTestCertPEM(t)
	now := time.Now()
	if err := h.storeResult("crt_g", "", []string{"example.com"}, certPEM, keyPEM, now, now.Add(30*24*time.Hour), "test", false); err != nil {
		t.Fatal(err)
	}

	if _, _, err := h.StartReissueTask("nope", []string{"example.com"}, "", ChallengeAuto); err == nil {
		t.Fatal("unknown id must be rejected")
	}
	if _, _, err := h.StartReissueTask("crt_g", nil, "", ChallengeAuto); err == nil || !strings.Contains(err.Error(), "至少需要一个域名") {
		t.Fatalf("empty domains must be rejected, got %v", err)
	}
	if _, _, err := h.StartReissueTask("crt_g", []string{"example.com"}, "missing-acc", ChallengeAuto); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("unknown account must be rejected, got %v", err)
	}
	if err := h.createPendingCert("crt_pend", "", []string{"pend.example.com"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.StartReissueTask("crt_pend", []string{"pend.example.com"}, "", ChallengeAuto); !errors.Is(err, ErrCertificateBeingIssued) {
		t.Fatalf("pending reissue must return ErrCertificateBeingIssued, got %v", err)
	}
	h.mu.Lock()
	h.certs["crt_g"].Renewing = true
	h.mu.Unlock()
	if _, _, err := h.StartReissueTask("crt_g", []string{"example.com"}, "", ChallengeAuto); err == nil || !strings.Contains(err.Error(), "续期中") {
		t.Fatalf("renewing reissue must be refused, got %v", err)
	}
}

func TestStoreResultPreservesChallenge(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := makeTestCertPEM(t)
	now := time.Now()
	if err := h.storeResult("crt_ch", "acc1", []string{"example.com"}, certPEM, keyPEM, now, now.Add(30*24*time.Hour), "test", false); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.certs["crt_ch"].Challenge = ChallengeDNS01
	h.mu.Unlock()
	if err := h.storeResult("crt_ch", "acc1", []string{"example.com"}, certPEM, keyPEM, now, now.Add(90*24*time.Hour), "test", false); err != nil {
		t.Fatal(err)
	}
	got, _ := h.Get("crt_ch")
	if got.Challenge != ChallengeDNS01 {
		t.Fatalf("Challenge = %q, want dns-01 preserved across storeResult", got.Challenge)
	}
}

func TestEffectiveChallenge(t *testing.T) {
	cases := []struct {
		in   ChallengePreference
		want ChallengePreference
	}{
		{"", ChallengeAuto},
		{ChallengeHTTP01, ChallengeHTTP01},
		{ChallengeDNS01, ChallengeDNS01},
		{ChallengeDNS01Manual, ChallengeAuto},
		{"bogus", ChallengeAuto},
	}
	for _, tc := range cases {
		c := &Certificate{Challenge: tc.in}
		if got := c.effectiveChallenge(); got != tc.want {
			t.Errorf("Challenge %q: effective = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := (*Certificate)(nil).effectiveChallenge(); got != ChallengeAuto {
		t.Errorf("nil cert: effective = %q, want auto", got)
	}
}

// TestStartManualReissueOrderStoresIntoSameRecord: a manual DNS-01 order
// started for an existing record confirms into that same record, keeping
// its auto-renew setting.
func TestStartManualReissueOrderStoresIntoSameRecord(t *testing.T) {
	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeDNSACME(t, certPEM, false)
	defer fake.close()
	h, _ := newManualTestHub(t, fake)
	stubDNSLookup(t)

	oldCertPEM, oldKeyPEM := makeTestCertPEM(t)
	now := time.Now()
	if err := h.storeResult("crt_man", "", []string{"example.com", "*.example.com"}, oldCertPEM, oldKeyPEM, now, now.Add(30*24*time.Hour), "test", true); err != nil {
		t.Fatal(err)
	}
	order, err := h.StartManualReissueOrder("crt_man", []string{"example.com", "*.example.com"}, "")
	if err != nil {
		t.Fatalf("StartManualReissueOrder: %v", err)
	}
	if order.CertID != "crt_man" {
		t.Fatalf("order.CertID = %q, want the existing record id", order.CertID)
	}
	if len(order.Records) != 2 {
		t.Fatalf("order records = %d, want 2 TXT records", len(order.Records))
	}

	crt, err := h.ConfirmManualDNSOrder(order.ID)
	if err != nil {
		t.Fatalf("ConfirmManualDNSOrder: %v", err)
	}
	if crt.ID != "crt_man" {
		t.Fatalf("confirmed cert ID = %q, want crt_man", crt.ID)
	}
	if !crt.AutoRenew {
		t.Fatal("manual reissue must preserve the record's auto-renew setting")
	}
	if strings.TrimSpace(crt.CertPEM) != strings.TrimSpace(string(certPEM)) {
		t.Fatal("confirmed material must be the CA-issued certificate")
	}
	got, _ := h.Get("crt_man")
	if got.Status != CertStatusIssued || got.Renewing {
		t.Fatalf("Status = %q Renewing = %v after confirm", got.Status, got.Renewing)
	}
	done, err := h.GetPendingOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != ManualOrderDone || done.CertID != "crt_man" {
		t.Fatalf("order status = %s cert = %q, want done/crt_man", done.Status, done.CertID)
	}
}

// TestConfirmManualReissueOrderDeletedTarget: confirming an order whose
// target record was deleted must fail instead of resurrecting the cert.
func TestConfirmManualReissueOrderDeletedTarget(t *testing.T) {
	certPEM, _ := makeTestCertPEM(t)
	fake := newFakeDNSACME(t, certPEM, false)
	defer fake.close()
	h, _ := newManualTestHub(t, fake)
	stubDNSLookup(t)

	oldCertPEM, oldKeyPEM := makeTestCertPEM(t)
	now := time.Now()
	if err := h.storeResult("crt_del", "", []string{"example.com", "*.example.com"}, oldCertPEM, oldKeyPEM, now, now.Add(30*24*time.Hour), "test", false); err != nil {
		t.Fatal(err)
	}
	order, err := h.StartManualReissueOrder("crt_del", []string{"example.com", "*.example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Delete("crt_del"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ConfirmManualDNSOrder(order.ID); err == nil || !strings.Contains(err.Error(), "已被删除") {
		t.Fatalf("confirm into a deleted target must fail, got %v", err)
	}
	if _, ok := h.Get("crt_del"); ok {
		t.Fatal("deleted record must not be resurrected by a confirm")
	}
}

func TestReissueCertHandlerGuards(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{Name: "acc1", DirectoryURL: "http://127.0.0.1:9/directory", IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := makeTestCertPEM(t)
	now := time.Now()
	if err := h.storeResult("crt_h", "", []string{"example.com"}, certPEM, keyPEM, now, now.Add(30*24*time.Hour), "test", false); err != nil {
		t.Fatal(err)
	}
	if err := h.createPendingCert("crt_pend", "", []string{"pend.example.com"}, ""); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	handlers := NewHandlers(h, nil, nil)
	handlers.Register(r.Group(""), r.Group(""))

	call := func(id string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/certs/"+id+"/reissue", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	mk := func(domains []string, account, challenge string) string {
		b, _ := json.Marshal(map[string]any{"domains": domains, "account_id": account, "challenge": challenge})
		return string(b)
	}

	if w := call("nope", mk([]string{"example.com"}, "", "")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status = %d, want 404", w.Code)
	}
	if w := call("crt_pend", mk([]string{"pend.example.com"}, "", "")); w.Code != http.StatusConflict {
		t.Fatalf("pending: status = %d, want 409", w.Code)
	}
	h.mu.Lock()
	h.certs["crt_h"].Renewing = true
	h.mu.Unlock()
	if w := call("crt_h", mk([]string{"example.com"}, "", "")); w.Code != http.StatusConflict {
		t.Fatalf("renewing: status = %d, want 409", w.Code)
	}
	h.mu.Lock()
	h.certs["crt_h"].Renewing = false
	h.mu.Unlock()
	if w := call("crt_h", mk([]string{"example.com"}, "", "bogus")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad challenge: status = %d, want 400", w.Code)
	}
	if w := call("crt_h", mk(nil, "", "")); w.Code != http.StatusBadRequest {
		t.Fatalf("empty domains: status = %d, want 400", w.Code)
	}
	if w := call("crt_h", mk([]string{"example.com"}, "missing-acc", "")); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown account: status = %d, want 400", w.Code)
	}
	// Happy path: accepted into a background task; the fake directory is
	// unreachable so the task fails afterwards, but the handler already
	// returned 202 with the task id.
	w := call("crt_h", mk([]string{"example.com"}, "", ""))
	if w.Code != http.StatusAccepted {
		t.Fatalf("happy path: status = %d, want 202, body = %s", w.Code, w.Body.String())
	}
	var res struct {
		TaskID string `json:"task_id"`
		CertID string `json:"cert_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.TaskID == "" || res.CertID != "crt_h" {
		t.Fatalf("response = %+v, want task_id + cert_id crt_h", res)
	}
}
