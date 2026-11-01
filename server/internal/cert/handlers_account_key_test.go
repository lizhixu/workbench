package cert

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRevealAccountKey pins the EAB HMAC key reveal endpoint: every routine
// response carries only the mask (ACMEAccount.Redacted), so the account
// dialog's eye button has exactly one door to the clear text. That door must
// stay narrow — unknown account and account-without-EAB both 404, and the list
// response must never leak the secret even after a reveal happened.
func TestRevealAccountKey(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "hmac-secret-value"
	if err := h.PutAccount(&ACMEAccount{
		ID:           "acc_eab",
		Name:         "Google Trust Services",
		DirectoryURL: "https://dv.acme-v02.api.pki.goog/directory",
		EABKeyID:     "kid-1",
		EABHMACKey:   secret,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutAccount(&ACMEAccount{
		ID:           "acc_plain",
		Name:         "Let's Encrypt",
		DirectoryURL: DefaultDirectoryURL,
	}); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandlers(h, nil, nil).Register(r.Group(""), r.Group(""))

	reveal := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w := reveal("/certs/accounts/acc_eab/key")
	if w.Code != http.StatusOK {
		t.Fatalf("reveal: status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			EABHMACKey string `json:"eab_hmac_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("reveal: decode body: %v", err)
	}
	if resp.Data.EABHMACKey != secret {
		t.Errorf("reveal: key = %q, want %q", resp.Data.EABHMACKey, secret)
	}

	if w := reveal("/certs/accounts/acc_plain/key"); w.Code != http.StatusNotFound {
		t.Errorf("account without EAB: status = %d, want 404", w.Code)
	}
	if w := reveal("/certs/accounts/missing/key"); w.Code != http.StatusNotFound {
		t.Errorf("unknown account: status = %d, want 404", w.Code)
	}

	for _, acc := range h.ListAccounts() {
		if strings.Contains(acc.EABHMACKey, secret) {
			t.Errorf("list response leaked the clear-text EAB HMAC key for %s", acc.ID)
		}
	}
}
