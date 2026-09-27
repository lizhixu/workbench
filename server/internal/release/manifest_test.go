package release

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `{
  "version": "v1.3.0",
  "commit": "abc123",
  "build_time": "2026-09-27T14:06:59Z",
  "agents": {
    "linux/amd64":   {"file": "bin/watchman-agent-linux-amd64",   "sha256": "aaa"},
    "linux/arm64":   {"file": "bin/watchman-agent-linux-arm64",   "sha256": "bbb"},
    "windows/amd64": {"file": "bin/watchman-agent-windows-amd64.exe", "sha256": "ccc"}
  }
}`

func writeSample(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, []byte(sample), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	m, err := Load(writeSample(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Version != "v1.3.0" || m.Commit != "abc123" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if sum, ok := m.AgentSha256("linux", "amd64"); !ok || sum != "aaa" {
		t.Fatalf("AgentSha256 linux/amd64 = %q, %v", sum, ok)
	}
	if _, ok := m.AgentSha256("darwin", "amd64"); ok {
		t.Fatal("AgentSha256 darwin/amd64 should not exist")
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadInvalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, []byte(`{"version": ""}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for empty version")
	}
}

func TestNilManifest(t *testing.T) {
	var m *Manifest
	if _, ok := m.AgentSha256("linux", "amd64"); ok {
		t.Fatal("nil manifest should return ok=false")
	}
}
