package install

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

