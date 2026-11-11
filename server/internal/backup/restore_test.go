package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
	"watchman/server/internal/vault"
)

func TestRestoreDamageCannotChangeLiveFiles(t *testing.T) {
	for _, kind := range []string{"truncated", "gzip-crc", "invalid-json", "reserved-path", "duplicate", "manifest-hash"} {
		t.Run(kind, func(t *testing.T) {
			s, dir := newTestStore(t)
			writeSeed(t, dir)
			original, _ := os.ReadFile(filepath.Join(dir, "users.json"))
			archive := buildArchive(t, map[string]string{"users.json": `[{"username":"restored"}]`})
			switch kind {
			case "truncated":
				var buf bytes.Buffer
				gz := gzip.NewWriter(&buf)
				tw := tar.NewWriter(gz)
				if err := tw.WriteHeader(&tar.Header{Name: "users.json", Size: 100, Mode: 0600}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write([]byte("cut")); err != nil {
					t.Fatal(err)
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
				archive = buf.Bytes()
			case "gzip-crc":
				archive[len(archive)-8] ^= 0xff
			case "invalid-json":
				archive = buildArchive(t, map[string]string{"users.json": "bad json"})
			case "reserved-path":
				archive = buildArchive(t, map[string]string{"Backups/restore-pending.json": "{}", "users.json": "[]"})
			case "duplicate":
				var buf bytes.Buffer
				gz := gzip.NewWriter(&buf)
				tw := tar.NewWriter(gz)
				for i := 0; i < 2; i++ {
					if err := tw.WriteHeader(&tar.Header{Name: "users.json", Size: 2, Mode: 0600}); err != nil {
						t.Fatal(err)
					}
					tw.Write([]byte("[]"))
				}
				tw.Close()
				gz.Close()
				archive = buf.Bytes()
			case "manifest-hash":
				archive = buildArchive(t, map[string]string{"users.json": "[]", manifestName: `{"format":"watchman-control-backup","format_version":1,"files":[{"path":"users.json","size":2,"sha256":"wrong"}]}`})
			}
			if _, err := s.Restore(bytes.NewReader(archive), "admin", kind); err == nil {
				t.Fatal("bad archive accepted")
			}
			got, _ := os.ReadFile(filepath.Join(dir, "users.json"))
			if !bytes.Equal(got, original) || s.hasPending() {
				t.Fatal("rejected restore modified live data or left pending transaction")
			}
		})
	}
}

func TestRestoreFileChecksStoredHash(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)
	m, err := s.Create("admin", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.backupDir, m.Name), buildArchive(t, map[string]string{"users.json": "[]"}), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFile(m.Name, "admin"); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("want hash mismatch: %v", err)
	}
}

func TestRestoreApplyRollsBackFailedReplacement(t *testing.T) {
	s, dir := newTestStore(t)
	for _, name := range []string{"users.json", "settings.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	archive := buildArchive(t, map[string]string{"users.json": "[]", "settings.json": `{"system":{}}`, "extra.txt": "new"})
	if _, err := s.Restore(bytes.NewReader(archive), "admin", ""); err != nil {
		t.Fatal(err)
	}
	s.rename = func(from, to string) error {
		if filepath.Base(to) == "users.json" && strings.Contains(from, string(filepath.Separator)+"files"+string(filepath.Separator)) {
			return fmt.Errorf("injected disk failure")
		}
		return os.Rename(from, to)
	}
	if err := s.ApplyPending(); err != nil {
		t.Fatalf("rollback should allow original state to boot: %v", err)
	}
	for _, name := range []string{"users.json", "settings.json"} {
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		if string(raw) != "{}" {
			t.Fatalf("%s not rolled back: %s", name, raw)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "extra.txt")); !os.IsNotExist(err) {
		t.Fatal("new file survived rollback")
	}
	if s.Status().Status != "failed" || s.hasPending() {
		t.Fatalf("bad failed transaction status: %+v", s.Status())
	}
}

func TestInterruptedRestoreRollsBackBeforeStartup(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)
	if _, err := s.Restore(bytes.NewReader(buildArchive(t, map[string]string{"users.json": "[]"})), "admin", ""); err != nil {
		t.Fatal(err)
	}
	p, _ := s.readPending()
	p.Phase = "applying"
	p.Files[0].Existed = true
	if err := atomicJSON(s.pendingPath(), p); err != nil {
		t.Fatal(err)
	}
	undo := filepath.Join(s.backupDir, p.ID, "undo")
	os.MkdirAll(undo, 0700)
	if err := os.Rename(filepath.Join(dir, "users.json"), filepath.Join(undo, "users.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.backupDir, p.ID, "files", "users.json"), filepath.Join(dir, "users.json")); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ApplyPending(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "users.json"))
	if !strings.Contains(string(raw), "admin") {
		t.Fatal("interrupted transaction not rolled back")
	}
	if restarted.Status().Status != "failed" {
		t.Fatal("interruption not reported")
	}
}

func TestRestoreStagingSurvivesAgentReconnect(t *testing.T) {
	s, dir := newTestStore(t)
	reg := rpc.NewRegistry(dir, nil)
	_, resp, err := reg.Register(context.Background(), &agentpb.RegisterRequest{AgentId: "host", Hostname: "fixture", EnrollToken: reg.IssueEnrollToken()}, "")
	if err != nil {
		t.Fatal(err)
	}
	reg.SetAgentGroup("host", "archived")
	m, err := s.Create("admin", "", false)
	if err != nil {
		t.Fatal(err)
	}
	reg.SetAgentGroup("host", "live")
	if _, err := s.RestoreFile(m.Name, "admin"); err != nil {
		t.Fatal(err)
	}
	reg.Register(context.Background(), &agentpb.RegisterRequest{AgentId: "host", Hostname: "fixture"}, resp.AuthToken)
	boot, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := boot.ApplyPending(); err != nil {
		t.Fatal(err)
	}
	if got := rpc.NewRegistry(dir, nil).GetAgent("host").Group; got != "archived" {
		t.Fatalf("restored group=%s", got)
	}
}

func TestCrossMachineRestorePreservesSecrets(t *testing.T) {
	s, dir := newTestStore(t)
	source, err := EnsureServerSecrets(dir, "source-jwt", "source-vault")
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.NewStore(dir, source.VaultPass)
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Create(&vault.Credential{Name: "fixture", Secret: "test-only"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Create("admin", "", false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.backupDir, m.Name))
	if err != nil {
		t.Fatal(err)
	}
	target, targetDir := newTestStore(t)
	if _, err := EnsureServerSecrets(targetDir, "other-jwt", "other-vault"); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Restore(bytes.NewReader(raw), "admin", ""); err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyPending(); err != nil {
		t.Fatal(err)
	}
	keys, err := EnsureServerSecrets(targetDir, "other-jwt", "other-vault")
	if err != nil || keys != source {
		t.Fatalf("migration identity lost: %v", err)
	}
	reloaded, err := vault.NewStore(targetDir, keys.VaultPass)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get(c.ID)
	if !ok || got.Secret != "test-only" {
		t.Fatal("credential lost after migration")
	}
}

func TestLegacyVaultRequiresCorrectKey(t *testing.T) {
	s, dir := newTestStore(t)
	EnsureServerSecrets(dir, "jwt", "destination")
	source := t.TempDir()
	v, _ := vault.NewStore(source, "original")
	v.Create(&vault.Credential{Name: "fixture", Secret: "test"})
	raw, _ := os.ReadFile(filepath.Join(source, "vault.json"))
	archive := buildArchive(t, map[string]string{"vault.json": string(raw)})
	if _, err := s.Restore(bytes.NewReader(archive), "admin", ""); err == nil {
		t.Fatal("wrong key accepted")
	}
	res, err := s.prepare(bytes.NewReader(archive), "admin", "legacy", "", "original")
	if err != nil || !res.Legacy {
		t.Fatalf("legacy migration failed: %v", err)
	}
	if err := s.ApplyPending(); err != nil {
		t.Fatal(err)
	}
	keys, err := EnsureServerSecrets(dir, "", "")
	if err != nil || keys.VaultPass != "original" {
		t.Fatal("legacy key not persisted")
	}
}

func TestConcurrentBackupsAreCompleteAndUnique(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)
	var wg sync.WaitGroup
	var mu sync.Mutex
	names := map[string]bool{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := s.Create("admin", "", false)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if names[m.Name] {
				t.Error("duplicate archive")
			}
			names[m.Name] = true
			raw, err := os.ReadFile(filepath.Join(s.backupDir, m.Name))
			if err != nil {
				t.Error(err)
				return
			}
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != m.SHA256 {
				t.Error("archive hash mismatch")
			}
		}()
	}
	wg.Wait()
	if len(names) != 8 {
		t.Fatalf("got %d archives", len(names))
	}
}

func TestPreparedRestoreRechecksStagingAndCanCancel(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)
	archive := buildArchive(t, map[string]string{"users.json": "[]"})
	if _, err := s.Restore(bytes.NewReader(archive), "admin", ""); err != nil {
		t.Fatal(err)
	}
	p, _ := s.readPending()
	os.WriteFile(filepath.Join(s.backupDir, p.ID, "files", "users.json"), []byte("{}"), 0600)
	if err := s.ApplyPending(); err != nil {
		t.Fatal(err)
	}
	if s.Status().Status != "failed" {
		t.Fatal("tampered staging accepted")
	}
	if _, err := s.Restore(bytes.NewReader(archive), "admin", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPending(); err != nil {
		t.Fatal(err)
	}
	if s.hasPending() {
		t.Fatal("cancel left pending transaction")
	}
}

func TestManifestIncludesFileHashes(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)
	m, err := s.Create("admin", "", false)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(filepath.Join(s.backupDir, m.Name))
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			t.Fatal("manifest missing")
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == manifestName {
			var m archiveManifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				t.Fatal(err)
			}
			if len(m.Files) != 2 || len(m.Files[0].SHA256) != 64 || m.ServerVersion == "" {
				t.Fatalf("bad manifest: %+v", m)
			}
			break
		}
	}
}

func TestRestoreRevalidatesVaultCreatedAfterPreparation(t *testing.T) {
	s, dir := newTestStore(t)
	if _, err := EnsureServerSecrets(dir, "old-jwt", "old-vault"); err != nil {
		t.Fatal(err)
	}
	keys, _ := json.Marshal(ServerSecrets{JWTKey: "new-jwt", VaultPass: "new-vault"})
	archive := buildArchive(t, map[string]string{"users.json": "[]", secretsFile: string(keys)})
	if _, err := s.Restore(bytes.NewReader(archive), "admin", ""); err != nil {
		t.Fatal(err)
	}
	v, err := vault.NewStore(dir, "old-vault")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Create(&vault.Credential{Name: "after-prepare", Secret: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPending(); err != nil {
		t.Fatal(err)
	}
	if s.Status().Status != "failed" {
		t.Fatal("restore accepted incompatible retained vault")
	}
	got, err := EnsureServerSecrets(dir, "", "")
	if err != nil || got.VaultPass != "old-vault" {
		t.Fatal("live vault identity changed")
	}
}
