package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, dir
}

func writeSeed(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(`[{"username":"admin"}]`), 0o600); err != nil {
		t.Fatalf("seed users.json: %v", err)
	}
	sub := filepath.Join(dir, "records")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir records: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "s1.cast"), []byte("cast-data"), 0o600); err != nil {
		t.Fatalf("seed recording: %v", err)
	}
}

func TestDisabledStoreReportsErrDisabled(t *testing.T) {
	s, err := NewStore("", nil)
	if err != nil {
		t.Fatalf("NewStore(\"\"): %v", err)
	}
	if _, err := s.Create("admin", "", false); err != ErrDisabled {
		t.Fatalf("Create on disabled store = %v, want ErrDisabled", err)
	}
	if _, err := s.Restore(bytes.NewReader(nil), "admin", ""); err != ErrDisabled {
		t.Fatalf("Restore on disabled store = %v, want ErrDisabled", err)
	}
	if _, err := s.Path("watchman-backup-x.tar.gz"); err != ErrDisabled {
		t.Fatalf("Path on disabled store = %v, want ErrDisabled", err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("List on disabled store = %d entries, want 0", len(got))
	}
}

func TestCreateArchivesDataDir(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)

	meta, err := s.Create("admin", "手动备份", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.Files != 2 {
		t.Fatalf("Files = %d, want 2 (users.json + records/s1.cast)", meta.Files)
	}
	if meta.Size <= 0 {
		t.Fatalf("Size = %d, want > 0", meta.Size)
	}
	if len(meta.SHA256) != 64 {
		t.Fatalf("SHA256 = %q, want 64 hex chars", meta.SHA256)
	}
	if meta.CreatedBy != "admin" || meta.Note != "手动备份" {
		t.Fatalf("metadata not recorded: %+v", meta)
	}

	names := archiveNames(t, filepath.Join(dir, backupsDirName, meta.Name))
	if !names["users.json"] || !names["records/s1.cast"] {
		t.Fatalf("archive contents = %v, want users.json + records/s1.cast", names)
	}
	for n := range names {
		if strings.HasPrefix(n, backupsDirName) {
			t.Fatalf("archive contains the backup dir itself: %q", n)
		}
	}
}

func TestCreateSkipsBackupDirSoArchivesDoNotNest(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)

	first, err := s.Create("admin", "first", false)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := s.Create("admin", "second", false)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}

	names := archiveNames(t, filepath.Join(dir, backupsDirName, second.Name))
	if names[backupsDirName+"/"+first.Name] {
		t.Fatal("second archive nested the first archive")
	}
	if names[backupsDirName+"/"+indexFileName] {
		t.Fatal("second archive included the backup index")
	}
	if got := len(s.List()); got != 2 {
		t.Fatalf("List = %d, want 2", got)
	}
}

func TestRestoreRoundTripAndSafetyCopy(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)

	meta, err := s.Create("admin", "before edit", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate damage after the backup was taken.
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte("CORRUPT"), 0o600); err != nil {
		t.Fatalf("corrupt users.json: %v", err)
	}

	res, err := s.RestoreFile(meta.Name, "admin")
	if err != nil {
		t.Fatalf("RestoreFile: %v", err)
	}
	if res.Files != 2 {
		t.Fatalf("restored Files = %d, want 2", res.Files)
	}
	if !res.RestartRequired {
		t.Fatal("RestartRequired = false, want true: in-memory stores still hold pre-restore state")
	}
	if res.SafetyCopy == "" {
		t.Fatal("SafetyCopy empty, want the pre-restore archive name")
	}

	got, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatalf("read restored users.json: %v", err)
	}
	if string(got) != `[{"username":"admin"}]` {
		t.Fatalf("users.json = %q, want the archived content", got)
	}

	// The safety copy must be a real, listed archive holding the damaged state.
	safetyPath, err := s.Path(res.SafetyCopy)
	if err != nil {
		t.Fatalf("Path(safety copy): %v", err)
	}
	if _, err := os.Stat(safetyPath); err != nil {
		t.Fatalf("safety copy missing: %v", err)
	}
	var listedSafety bool
	for _, m := range s.List() {
		if m.Name == res.SafetyCopy {
			listedSafety = true
			if !m.Auto {
				t.Fatal("safety copy not marked Auto")
			}
		}
	}
	if !listedSafety {
		t.Fatal("safety copy not present in List()")
	}
}

func TestRestoreRejectsPathTraversal(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)

	outside := filepath.Join(filepath.Dir(dir), "escaped.txt")
	archive := buildArchive(t, map[string]string{"../escaped.txt": "pwned"})

	if _, err := s.Restore(bytes.NewReader(archive), "admin", "malicious"); err == nil {
		t.Fatal("Restore accepted a ../ entry, want rejection")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("tar slip wrote outside the data dir: %s", outside)
	}
}

func TestRestoreRejectsAbsoluteEntry(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)

	archive := buildArchive(t, map[string]string{"/etc/watchman-owned": "pwned"})
	if _, err := s.Restore(bytes.NewReader(archive), "admin", "malicious"); err == nil {
		t.Fatal("Restore accepted an absolute entry, want rejection")
	}
}

func TestRestoreRejectsNonGzip(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)

	if _, err := s.Restore(strings.NewReader("this is not a gzip archive"), "admin", "junk"); err == nil {
		t.Fatal("Restore accepted a non-gzip body, want rejection")
	}
}

func TestPathRejectsSuspiciousNames(t *testing.T) {
	s, _ := newTestStore(t)
	for _, name := range []string{
		"",
		"../users.json",
		"backups/../users.json",
		`..\users.json`,
		"users.json",
		"watchman-backup-x.zip",
		"random.tar.gz",
	} {
		if _, err := s.Path(name); err == nil {
			t.Fatalf("Path(%q) accepted, want rejection", name)
		}
	}
}

func TestDeleteRemovesArchiveAndIndexEntry(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)

	meta, err := s.Create("admin", "", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(meta.Name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, backupsDirName, meta.Name)); !os.IsNotExist(err) {
		t.Fatalf("archive still on disk after Delete: %v", err)
	}
	if got := len(s.List()); got != 0 {
		t.Fatalf("List = %d after Delete, want 0", got)
	}
	if err := s.Delete(meta.Name); err == nil {
		t.Fatal("second Delete succeeded, want not-found error")
	}
}

func TestReloadAdoptsExternalArchiveAndDropsMissingOne(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)

	meta, err := s.Create("admin", "", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Someone deletes an indexed archive and copies another one in by hand.
	if err := os.Remove(filepath.Join(dir, backupsDirName, meta.Name)); err != nil {
		t.Fatalf("remove archive: %v", err)
	}
	external := filepath.Join(dir, backupsDirName, archivePrefix+"19990101-000000"+archiveSuffix)
	if err := os.WriteFile(external, buildArchive(t, map[string]string{"users.json": "x"}), 0o600); err != nil {
		t.Fatalf("write external archive: %v", err)
	}

	s2, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	list := s2.List()
	if len(list) != 1 {
		t.Fatalf("List = %d, want 1 (external adopted, deleted one dropped)", len(list))
	}
	if list[0].Name != filepath.Base(external) {
		t.Fatalf("adopted archive = %q, want %q", list[0].Name, filepath.Base(external))
	}
}

// archiveNames returns the entry names inside a gzipped tar.
func archiveNames(t *testing.T, p string) map[string]bool {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()

	names := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		names[hdr.Name] = true
	}
	return names
}

// buildArchive produces a gzipped tar with the given entries, used to feed
// Restore hostile or hand-made archives.
func buildArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o600,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}
