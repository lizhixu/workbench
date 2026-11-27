package vault

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestRouter(s *Store, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("role", role)
		c.Next()
	})
	NewHandlers(s).Register(r.Group("/api/v1"))
	return r
}

func TestCredentialHandlersMaskAndReveal(t *testing.T) {
	s, err := NewStore(t.TempDir(), "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(&Credential{Name: "db", Type: "database", Secret: "s3cr3t"})
	if err != nil {
		t.Fatal(err)
	}
	r := newTestRouter(s, "admin")

	// Routine GET must never leak the plaintext.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/vault/credentials/"+created.ID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "s3cr3t") {
		t.Errorf("GET leaked secret: %s", w.Body.String())
	}
	var getResp struct {
		HasSecret bool `json:"has_secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &getResp); err != nil || !getResp.HasSecret {
		t.Errorf("GET has_secret missing: %v %s", err, w.Body.String())
	}

	// The explicit reveal endpoint returns the plaintext.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/vault/credentials/"+created.ID+"/secret", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "s3cr3t") {
		t.Errorf("reveal status=%d body=%s", w.Code, w.Body.String())
	}

	// Unknown credential -> 404.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/vault/credentials/missing/secret", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("reveal unknown status = %d", w.Code)
	}
}

func TestRevealSecretRequiresAdmin(t *testing.T) {
	s, err := NewStore(t.TempDir(), "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(&Credential{Name: "db", Type: "database", Secret: "s3cr3t"})
	if err != nil {
		t.Fatal(err)
	}
	r := newTestRouter(s, "operator")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/vault/credentials/"+created.ID+"/secret", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin reveal status = %d, want 403", w.Code)
	}
}
