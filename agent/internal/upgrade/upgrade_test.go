package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"watchman/proto/agentpb"
)

type mockSender struct {
	messages []*agentpb.AgentMessage
}

func (s *mockSender) Send(msg *agentpb.AgentMessage) bool {
	s.messages = append(s.messages, msg)
	return true
}

func TestReplaceSelf(t *testing.T) {
	dir := t.TempDir()
	origBin := filepath.Join(dir, "agent-dummy")
	if err := os.WriteFile(origBin, []byte("version-1"), 0o755); err != nil {
		t.Fatalf("create orig: %v", err)
	}

	newBin := filepath.Join(dir, "agent-new")
	if err := os.WriteFile(newBin, []byte("version-2-content"), 0o755); err != nil {
		t.Fatalf("create new: %v", err)
	}

	m := NewManager(slog.Default())
	// Test staging and copy
	staging := filepath.Join(dir, ".agent-dummy.new")
	if err := copyFile(newBin, staging); err != nil {
		t.Fatalf("copyFile failed: %v", err)
	}
	if err := os.Rename(staging, origBin); err != nil {
		t.Fatalf("rename failed: %v", err)
	}

	content, err := os.ReadFile(origBin)
	if err != nil {
		t.Fatalf("read replaced file: %v", err)
	}
	if string(content) != "version-2-content" {
		t.Errorf("content = %q, want version-2-content", string(content))
	}
	_ = m
}

func TestDoUpgrade_RejectsCorruptBinary(t *testing.T) {
	// Dummy corrupt payload
	corruptPayload := []byte("this is definitely not a compiled executable binary")
	sum := sha256.Sum256(corruptPayload)
	sumHex := hex.EncodeToString(sum[:])

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(corruptPayload)
	}))
	defer server.Close()

	sender := &mockSender{}
	m := NewManager(slog.Default())
	m.SetSender(sender)

	req := &agentpb.UpgradeRequest{
		Version: "v1.2.3",
		Url:     server.URL,
		Sha256:  sumHex,
	}

	m.doUpgrade(req)

	// Check that progress contains error from binary validation
	foundError := false
	for _, msg := range sender.messages {
		if p := msg.GetUpgradeProgress(); p != nil {
			if p.GetStage() == "error" && strings.Contains(p.GetError(), "binary validation rejected") {
				foundError = true
				break
			}
		}
	}

	if !foundError {
		t.Fatalf("expected binary validation error in progress messages, got: %+v", sender.messages)
	}
}

