package upgrade

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

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
