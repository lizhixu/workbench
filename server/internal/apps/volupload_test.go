package apps

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestUploadStore(t *testing.T) *VolumeUploadStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewVolumeUploadStore(dir, slog.Default())
	if err != nil {
		t.Fatalf("NewVolumeUploadStore: %v", err)
	}
	return s
}

func TestVolumeUploadTokenSingleUse(t *testing.T) {
	s := newTestUploadStore(t)
	tok, err := s.IssueToken("app1", "vbk_test1")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if _, _, ok := s.ConsumeToken(tok); !ok {
		t.Fatalf("first consume must succeed")
	}
	if _, _, ok := s.ConsumeToken(tok); ok {
		t.Fatalf("second consume must fail: token is single-use")
	}
	if _, _, ok := s.ConsumeToken("bogus"); ok {
		t.Fatalf("unknown token must fail")
	}
}

func TestVolumeUploadTokenExpiry(t *testing.T) {
	s := newTestUploadStore(t)
	tok, err := s.IssueToken("app1", "vbk_test2")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	s.mu.Lock()
	s.tokens[tok].expiresAt = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	if _, _, ok := s.ConsumeToken(tok); ok {
		t.Fatalf("expired token must fail")
	}
}

func TestVolumeUploadSaveSizeCap(t *testing.T) {
	s := newTestUploadStore(t)
	// The cap logic is exercised with a small injected limit (the real
	// 2GB cap would need gigabytes of test I/O); Save itself just passes
	// the package constant through.
	big := bytes.NewReader(make([]byte, 65))
	if _, err := s.saveWithLimit("vbk_big", big, 64); err == nil {
		t.Fatalf("oversize upload must fail")
	} else if err != errVolumeUploadTooLarge {
		t.Fatalf("oversize upload must report the cap, got %v", err)
	}
	if _, err := os.Stat(s.Path("vbk_big")); !os.IsNotExist(err) {
		t.Fatalf("oversize upload must not leave a partial file")
	}
	// Exactly at the limit passes.
	if n, err := s.saveWithLimit("vbk_ok", bytes.NewReader(make([]byte, 64)), 64); err != nil || n != 64 {
		t.Fatalf("at-limit upload must pass, got n=%d err=%v", n, err)
	}
}

func TestVolumeUploadServeRemovesFile(t *testing.T) {
	s := newTestUploadStore(t)
	if _, err := s.Save("vbk_srv", strings.NewReader("fake-tar-bytes")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	w := httptest.NewRecorder()
	if err := s.Serve(w, "vbk_srv"); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if w.Body.String() != "fake-tar-bytes" {
		t.Fatalf("Serve body mismatch: %q", w.Body.String())
	}
	if _, err := os.Stat(s.Path("vbk_srv")); !os.IsNotExist(err) {
		t.Fatalf("served file must be removed after transfer")
	}
}

func TestVolumeUploadStorePurgesStaleFiles(t *testing.T) {
	dir := t.TempDir()
	// Simulate a previous process's leftover.
	if err := os.MkdirAll(filepath.Join(dir, volumeUploadsDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, volumeUploadsDir, "vbk_old.tar.gz"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := NewVolumeUploadStore(dir, slog.Default()); err != nil {
		t.Fatalf("NewVolumeUploadStore: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, volumeUploadsDir)); len(entries) != 0 {
		t.Fatalf("startup must purge stale staged files, left %d", len(entries))
	}
}

func TestVolumeUploadDropTokenByArchive(t *testing.T) {
	s := newTestUploadStore(t)
	tok, err := s.IssueToken("app1", "vbk_drop")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	s.DropTokenByArchive("vbk_drop")
	if _, _, ok := s.ConsumeToken(tok); ok {
		t.Fatalf("dropped token must fail")
	}
}

func TestVolumeUploadTokenBindsFullRecord(t *testing.T) {
	s := newTestUploadStore(t)
	tok, err := s.IssueTokenForRecord("app1", "vbk_1", VolumeArchiveSourceUpload, VolumeBackupUploaded)
	if err != nil {
		t.Fatalf("IssueTokenForRecord: %v", err)
	}
	if s.ConsumeTokenForRecord(tok, "app2", "vbk_1", VolumeArchiveSourceUpload, VolumeBackupUploaded) {
		t.Fatal("token must reject another app")
	}
	if s.ConsumeTokenForRecord(tok, "app1", "vbk_2", VolumeArchiveSourceUpload, VolumeBackupUploaded) {
		t.Fatal("token must reject another archive")
	}
	if s.ConsumeTokenForRecord(tok, "app1", "vbk_1", "backup", VolumeBackupUploaded) {
		t.Fatal("token must reject another source")
	}
	if s.ConsumeTokenForRecord(tok, "app1", "vbk_1", VolumeArchiveSourceUpload, VolumeBackupSuccess) {
		t.Fatal("token must reject another lifecycle status")
	}
	if !s.ConsumeTokenForRecord(tok, "app1", "vbk_1", VolumeArchiveSourceUpload, VolumeBackupUploaded) {
		t.Fatal("correct full record binding must consume token")
	}
	if s.ConsumeTokenForRecord(tok, "app1", "vbk_1", VolumeArchiveSourceUpload, VolumeBackupUploaded) {
		t.Fatal("consumed token must not be replayed")
	}
}

func TestVolumeUploadLookupTokenReturnsFullBinding(t *testing.T) {
	s := newTestUploadStore(t)
	tok, err := s.IssueTokenForRecord("app1", "vbk_1", VolumeArchiveSourceUpload, VolumeBackupUploaded)
	if err != nil {
		t.Fatal(err)
	}
	archiveID, appID, source, status, ok := s.peekToken(tok)
	if !ok || archiveID != "vbk_1" || appID != "app1" || source != VolumeArchiveSourceUpload || status != VolumeBackupUploaded {
		t.Fatalf("peekToken = (%q,%q,%q,%q,%v)", archiveID, appID, source, status, ok)
	}
}

func TestUploadRestoreBaseURLRequiresTrustedHTTPURL(t *testing.T) {
	for _, raw := range []string{
		"",
		"//panel.example.com",
		"ftp://panel.example.com",
		"https://user:pass@panel.example.com",
		"https://panel.example.com/path?token=x",
		"https://panel.example.com#fragment",
		"https://:443",
		"https://panel.example.com:70000",
		"https://panel.example.com:0",
	} {
		if got, err := uploadRestoreBaseURL(raw); err == nil {
			t.Errorf("uploadRestoreBaseURL(%q) = %q, want error", raw, got)
		}
	}
	for raw, want := range map[string]string{
		"https://panel.example.com/": "https://panel.example.com",
		"http://panel.example.com:18789/base": "http://panel.example.com:18789/base",
	} {
		got, err := uploadRestoreBaseURL(raw)
		if err != nil || got != want {
			t.Errorf("uploadRestoreBaseURL(%q) = (%q,%v), want (%q,nil)", raw, got, err, want)
		}
	}
}
