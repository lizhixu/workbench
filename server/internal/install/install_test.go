package install

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestFindAgentBinary(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "watchman-install-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Case 1: Binary not present
	_, err = FindAgentBinary(tmpDir, "linux", "amd64")
	if err == nil {
		// If running in repository with bin/watchman-agent present, it might find repo fallback, which is okay.
		t.Logf("FindAgentBinary found fallback binary")
	}

	// Case 2: Specific custom binary in binDir
	customBin := filepath.Join(tmpDir, "watchman-agent-linux-arm64")
	if err := os.WriteFile(customBin, []byte("fake-arm64-binary"), 0755); err != nil {
		t.Fatalf("write custom bin: %v", err)
	}

	found, err := FindAgentBinary(tmpDir, "linux", "arm64")
	if err != nil {
		t.Fatalf("expected to find arm64 binary: %v", err)
	}
	if found != customBin {
		t.Errorf("expected %s, got %s", customBin, found)
	}

	// Case 3: Windows binary in binDir
	winBin := filepath.Join(tmpDir, "watchman-agent-windows-amd64.exe")
	if err := os.WriteFile(winBin, []byte("fake-win-binary"), 0755); err != nil {
		t.Fatalf("write win bin: %v", err)
	}

	foundWin, err := FindAgentBinary(tmpDir, "windows", "amd64")
	if err != nil {
		t.Fatalf("expected to find windows binary: %v", err)
	}
	if foundWin != winBin {
		t.Errorf("expected %s, got %s", winBin, foundWin)
	}

	// Test AgentBinarySha256
	sha, err := AgentBinarySha256(tmpDir, "linux", "arm64")
	if err != nil {
		t.Fatalf("AgentBinarySha256 error: %v", err)
	}
	if len(sha) != 64 {
		t.Errorf("expected 64 hex characters, got %s", sha)
	}
}

func TestAgentBinary_RejectsCorruptFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tmpDir := t.TempDir()

	// Place a corrupt 100-byte agent file
	corruptAgent := filepath.Join(tmpDir, "watchman-agent-linux-arm64")
	if err := os.WriteFile(corruptAgent, []byte("this is corrupted text data"), 0755); err != nil {
		t.Fatalf("write file: %v", err)
	}

	h := NewHandler(nil, tmpDir)
	engine := gin.New()
	h.RegisterRoutes(engine)

	req := httptest.NewRequest("GET", "/api/v1/agent/binary?os=linux&arch=arm64", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	// Since the file is corrupt, ValidateFormat should fail and reject serving it
	if w.Code == http.StatusOK {
		t.Fatalf("expected error for corrupted agent binary, got HTTP 200")
	}
}

func TestInstallScriptIgnoresForwardedProto(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, "")
	engine := gin.New()
	h.RegisterRoutes(engine)

	// Forged header + plaintext request: the script must still use http,
	// because the panel is the outermost service and the header is never
	// trusted. An https script here would make agents try to download
	// from a TLS endpoint the panel does not serve.
	req := httptest.NewRequest("GET", "http://10.0.0.5:18789/install?token=test-token-123", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("install script: got status %d, want 200", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, `WATCHMAN_SERVER="http://10.0.0.5:18789"`) {
		t.Fatalf("forwarded https: script does not contain http server URL")
	}
	if body := w.Body.String(); strings.Contains(body, `WATCHMAN_SERVER="https://10.0.0.5:18789"`) {
		t.Fatalf("forwarded https: script must not contain https server URL")
	}

	// Direct TLS still yields https.
	req = httptest.NewRequest("GET", "http://10.0.0.5:18789/install?token=test-token-123", nil)
	req.TLS = &tls.ConnectionState{}
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("direct TLS install script: got status %d, want 200", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, `WATCHMAN_SERVER="https://10.0.0.5:18789"`) {
		t.Fatalf("direct TLS: script does not contain https server URL")
	}
}
