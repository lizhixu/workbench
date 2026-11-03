package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/rpc"
	"watchman/server/internal/settings"
	"watchman/server/internal/snapshots"
)

// newUploadRestoreTestEnv wires the upload-scan/commit endpoints end to end: a
// registry with a fake online agent (the commit handler's hub gate), settings
// with a bound panel domain (the agent download URL gate), and an engine whose
// own registry stays nil so the async batch run fails fast offline instead of
// parking on a stream-less exec until its timeout.
func newUploadRestoreTestEnv(t *testing.T, agentOS string, bindDomain bool) (*gin.Engine, *Engine, *Store, *VolumeStore, *VolumeUploadStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	vols, err := NewVolumeStore(store.Dir(), nil)
	if err != nil {
		t.Fatalf("NewVolumeStore: %v", err)
	}
	us, err := NewVolumeUploadStore(store.Dir(), nil)
	if err != nil {
		t.Fatalf("NewVolumeUploadStore: %v", err)
	}
	reg := rpc.NewRegistry(t.TempDir(), nil)
	registerMigrateTestAgent(t, reg, "h1", agentOS)
	e := NewEngine(nil, store, nil, nil)
	e.SetVolumeStore(vols)
	e.SetVolumeUploadStore(us)
	h := NewAppHandlers(reg, store, e)
	s, err := settings.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}
	h.SetSettings(s)
	if bindDomain {
		if err := s.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
			"security.panel_domain": json.RawMessage(`"panel.example.com"`),
		}); err != nil {
			t.Fatalf("bind panel domain: %v", err)
		}
	}
	r := gin.New()
	h.RegisterAppRoutes(r.Group(""), r.Group(""), r.Group(""))
	return r, e, store, vols, us
}

type uploadPart struct {
	name     string
	value    string
	isFile   bool
	filename string
}

func buildUploadBody(t *testing.T, parts []uploadPart) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.isFile {
			fw, err := w.CreateFormFile(p.name, p.filename)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fw.Write([]byte(p.value)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := w.WriteField(p.name, p.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func postMultipart(t *testing.T, r *gin.Engine, path string, parts []uploadPart) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := buildUploadBody(t, parts)
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func postJSON(t *testing.T, r *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// filePart builds one uploaded file part; bytes are binary so use helper
// builders below for real tars.
func filePart(name, filename string, content []byte) uploadPart {
	return uploadPart{name: name, value: string(content), isFile: true, filename: filename}
}

// writeTestTar packs entries (name → content) into a plain uncompressed tar,
// exactly what `docker cp` piped out looks like.
func writeTestTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTestTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeTestSnapshotTar packs a multi-volume snapshot in the panel's own layout
// (manifest.json + volumes/vol_<i>), matching what volumeMultiBackupScript
// produces.
func writeTestSnapshotTar(t *testing.T, manifest *SnapshotManifest) []byte {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{"manifest.json": string(raw)}
	for i := range manifest.Volumes {
		entries[fmt.Sprintf("volumes/vol_%d/file.bin", i)] = fmt.Sprintf("vol-%d-data", i)
	}
	return writeTestTarGz(t, entries)
}

type scanRespItem struct {
	ArchiveID       string              `json:"archive_id"`
	Filename        string              `json:"filename"`
	Size            int64               `json:"size"`
	SHA256          string              `json:"sha256"`
	Snapshot        bool                `json:"snapshot"`
	TopDirs         []string            `json:"top_dirs"`
	ManifestVolumes []VolumeItemArchive `json:"manifest_volumes"`
	Match           string              `json:"match"`
	Matched         []volumeScanMatch   `json:"matched"`
}

func decodeScan(t *testing.T, w *httptest.ResponseRecorder) []scanRespItem {
	t.Helper()
	var resp struct {
		OK    bool           `json:"ok"`
		Items []scanRespItem `json:"items"`
		Error string         `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode scan body %q: %v", w.Body.String(), err)
	}
	return resp.Items
}

func putTestApp(t *testing.T, store *Store, volumes []string) {
	t.Helper()
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: volumes}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
}

// ---- scan：内容识别 ----

func TestScanVolumeUploadsAutoMatch(t *testing.T) {
	r, _, store, _, us := newUploadRestoreTestEnv(t, "linux", false)
	putTestApp(t, store, []string{"/opt/appdata:/data", "/opt/mysql:/var/lib/mysql", "/opt/logs:/srv/logs"})

	// docker-cp-shaped files: "data/…" hits container path /data exactly;
	// "mysql/…" hits both /var/lib/mysql (container basename) — the third
	// volume /srv/logs also shares basename "logs" but no entry does, so it
	// must not be proposed.
	rawTar := writeTestTar(t, map[string]string{"data/db.sqlite": "x", "data/conf/app.conf": "y"})
	gzTar := writeTestTarGz(t, map[string]string{"mysql/ibdata1": "z", "other.txt": "w"})
	noMatch := writeTestTar(t, map[string]string{"whatever/keep": "v"})
	snapshot := writeTestSnapshotTar(t, &SnapshotManifest{
		Version: "1.0", AppID: "app1", ArchiveID: "vbk_src",
		Volumes: []VolumeItemArchive{
			{VolumeSpec: "/opt/logs:/srv/logs", VolumeType: "bind", HostPath: "/opt/logs", ContainerPath: "/srv/logs"},
			{VolumeSpec: "/opt/appdata:/data", VolumeType: "bind", HostPath: "/opt/appdata", ContainerPath: "/data"},
		},
	})

	w := postMultipart(t, r, "/apps/app1/volume-backups/upload-scan", []uploadPart{
		filePart("file", "data.tar", rawTar),
		filePart("file", "mysql.tar.gz", gzTar),
		filePart("file", "nomatch.tar", noMatch),
		filePart("file", "snapshot.tar.gz", snapshot),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	items := decodeScan(t, w)
	if len(items) != 4 {
		t.Fatalf("items=%d, want 4", len(items))
	}

	if items[0].Match != scanMatchUnique || len(items[0].Matched) != 1 || items[0].Matched[0].VolumeSpec != "/opt/appdata:/data" || items[0].Matched[0].Reason != "container_path" {
		t.Fatalf("raw data.tar match = %q %+v, want unique /opt/appdata:/data by container_path", items[0].Match, items[0].Matched)
	}
	if items[0].SHA256 == "" || len(items[0].SHA256) != 64 {
		t.Fatalf("server-computed sha256 missing: %q", items[0].SHA256)
	}
	if items[0].Size != int64(len(rawTar)) {
		t.Fatalf("size=%d want %d", items[0].Size, len(rawTar))
	}
	if items[1].Match != scanMatchUnique || items[1].Matched[0].VolumeSpec != "/opt/mysql:/var/lib/mysql" {
		t.Fatalf("mysql.tar.gz match = %q %+v, want unique /opt/mysql:/var/lib/mysql", items[1].Match, items[1].Matched)
	}
	if items[2].Match != scanMatchNone || len(items[2].Matched) != 0 {
		t.Fatalf("nomatch.tar match = %q %+v, want none", items[2].Match, items[2].Matched)
	}
	if !items[3].Snapshot || items[3].Match != scanMatchManifest || len(items[3].Matched) != 2 {
		t.Fatalf("snapshot match = %q snapshot=%v matched=%+v, want manifest/2", items[3].Match, items[3].Snapshot, items[3].Matched)
	}
	if len(items[3].ManifestVolumes) != 2 {
		t.Fatalf("snapshot manifest volumes = %+v, want 2 items", items[3].ManifestVolumes)
	}

	// All four staged and registered; nothing restored yet (no slot taken,
	// no archive records created).
	entries, err := os.ReadDir(us.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("staged files = %d, want 4", len(entries))
	}
	if _, ok := us.PeekScan("app1", items[0].ArchiveID); !ok {
		t.Fatal("scan entry missing after scan")
	}
}

func TestScanVolumeUploadsAmbiguous(t *testing.T) {
	r, _, store, _, _ := newUploadRestoreTestEnv(t, "linux", false)
	putTestApp(t, store, []string{"/opt/mysql:/var/lib/mysql", "/opt/mysql2:/srv/mysql"})
	tar1 := writeTestTar(t, map[string]string{"mysql/ibdata1": "z"})
	w := postMultipart(t, r, "/apps/app1/volume-backups/upload-scan", []uploadPart{filePart("file", "a.tar", tar1)})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	items := decodeScan(t, w)
	if items[0].Match != scanMatchAmbiguous || len(items[0].Matched) != 2 {
		t.Fatalf("match=%q matched=%+v, want ambiguous with both candidates", items[0].Match, items[0].Matched)
	}
}

func TestScanVolumeUploadsValidation(t *testing.T) {
	cases := []struct {
		name    string
		parts   []uploadPart
		wantMsg string
	}{
		{"空请求体", nil, "请上传备份文件"},
		{"非 tar 内容", []uploadPart{filePart("file", "x.tar", []byte("not a tar at all"))}, "无法识别"},
		{"超上限", []uploadPart{
			filePart("file", "a.tar", writeTestTar(t, map[string]string{"data/x": "1"})),
			filePart("file", "b.tar", writeTestTar(t, map[string]string{"data/x": "2"})),
			filePart("file", "c.tar", writeTestTar(t, map[string]string{"data/x": "3"})),
			filePart("file", "d.tar", writeTestTar(t, map[string]string{"data/x": "4"})),
			filePart("file", "e.tar", writeTestTar(t, map[string]string{"data/x": "5"})),
			filePart("file", "f.tar", writeTestTar(t, map[string]string{"data/x": "6"})),
			filePart("file", "g.tar", writeTestTar(t, map[string]string{"data/x": "7"})),
			filePart("file", "h.tar", writeTestTar(t, map[string]string{"data/x": "8"})),
			filePart("file", "i.tar", writeTestTar(t, map[string]string{"data/x": "9"})),
			filePart("file", "j.tar", writeTestTar(t, map[string]string{"data/x": "10"})),
			filePart("file", "k.tar", writeTestTar(t, map[string]string{"data/x": "11"})),
		}, "一次最多上传"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _, store, _, us := newUploadRestoreTestEnv(t, "linux", false)
			putTestApp(t, store, []string{"/opt/a:/a"})
			w := postMultipart(t, r, "/apps/app1/volume-backups/upload-scan", tc.parts)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s, want 400", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantMsg) {
				t.Fatalf("body=%s, want it to contain %q", w.Body.String(), tc.wantMsg)
			}
			entries, err := os.ReadDir(us.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("rejected scan left staged files behind: %v", entries)
			}
		})
	}
}

func TestScanVolumeUploadsIgnoresStrayFieldsAndWindows(t *testing.T) {
	// Stray non-file fields are drained, not fatal.
	r, _, store, _, _ := newUploadRestoreTestEnv(t, "linux", false)
	putTestApp(t, store, []string{"/opt/a:/a"})
	w := postMultipart(t, r, "/apps/app1/volume-backups/upload-scan", []uploadPart{
		{name: "volume_spec", value: "/opt/a:/a"},
		filePart("file", "a.tar", writeTestTar(t, map[string]string{"a/x": "1"})),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200 (stray fields ignored)", w.Code, w.Body.String())
	}
	// Windows host is rejected up front like every other volume op.
	rw, _, sw, _, _ := newUploadRestoreTestEnv(t, "windows", false)
	putTestApp(t, sw, []string{"/opt/a:/a"})
	ww := postMultipart(t, rw, "/apps/app1/volume-backups/upload-scan", []uploadPart{
		filePart("file", "a.tar", writeTestTar(t, map[string]string{"a/x": "1"})),
	})
	if ww.Code != http.StatusBadRequest || !strings.Contains(ww.Body.String(), "暂仅支持 Linux 主机") {
		t.Fatalf("code=%d body=%s, want the Linux-only rejection", ww.Code, ww.Body.String())
	}
}

// ---- commit：确认并批量恢复 ----

func scanOneFile(t *testing.T, r *gin.Engine, content []byte, filename string) scanRespItem {
	t.Helper()
	w := postMultipart(t, r, "/apps/app1/volume-backups/upload-scan", []uploadPart{filePart("file", filename, content)})
	if w.Code != http.StatusOK {
		t.Fatalf("scan code=%d body=%s", w.Code, w.Body.String())
	}
	items := decodeScan(t, w)
	if len(items) != 1 {
		t.Fatalf("scan items=%d, want 1", len(items))
	}
	return items[0]
}

func TestCommitVolumeUploadRestore(t *testing.T) {
	r, e, store, vols, us := newUploadRestoreTestEnv(t, "linux", true)
	putTestApp(t, store, []string{"/opt/a:/a", "/opt/b:/b"})

	// File 1 auto-matches /a by content; file 2 matches nothing and the
	// operator picks /b by hand — both paths through one commit.
	it1 := scanOneFile(t, r, writeTestTar(t, map[string]string{"a/keep": "1"}), "a.tar")
	it2 := scanOneFile(t, r, writeTestTar(t, map[string]string{"whatever/keep": "2"}), "b.tar")

	w := postJSON(t, r, "/apps/app1/volume-backups/upload-commit", map[string]any{
		"pairs": []map[string]any{
			{"archive_id": it1.ArchiveID, "specs": []string{it1.Matched[0].VolumeSpec}},
			{"archive_id": it2.ArchiveID, "specs": []string{"/opt/b:/b"}},
		},
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202", w.Code, w.Body.String())
	}
	var resp struct {
		ArchiveID  string   `json:"archive_id"`
		ArchiveIDs []string `json:"archive_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202 body: %v (%s)", err, w.Body.String())
	}
	if len(resp.ArchiveIDs) != 2 || resp.ArchiveID != resp.ArchiveIDs[0] {
		t.Fatalf("archive_id=%q archive_ids=%v, want 2 ids with archive_id as the first", resp.ArchiveID, resp.ArchiveIDs)
	}
	for i, id := range resp.ArchiveIDs {
		arch, ok := vols.GetArchive("app1", id)
		if !ok {
			t.Fatalf("record %s not persisted", id)
		}
		want := [2]string{"/opt/a", "/a"}
		if i == 1 {
			want = [2]string{"/opt/b", "/b"}
		}
		if arch.HostPath != want[0] || arch.ContainerPath != want[1] {
			t.Fatalf("record %s host=%q container=%q, want %v", id, arch.HostPath, arch.ContainerPath, want)
		}
		if arch.Source != VolumeArchiveSourceUpload || arch.Status != VolumeBackupUploaded {
			t.Fatalf("record %s source=%q status=%q", id, arch.Source, arch.Status)
		}
		if arch.SHA256 == "" || len(arch.SHA256) != 64 {
			t.Fatalf("record %s missing server-computed sha: %q", id, arch.SHA256)
		}
	}
	// The engine registry is nil, so the async batch fails fast with the
	// offline sentinel: slot back, both records failed, staging cleaned.
	waitFor(t, 5*time.Second, func() bool {
		if e.VolumeBusy("app1") {
			return false
		}
		for _, id := range resp.ArchiveIDs {
			arch, ok := vols.GetArchive("app1", id)
			if !ok || arch.LastRestoreStatus != VolumeRestoreFailed {
				return false
			}
		}
		entries, _ := os.ReadDir(us.dir)
		return len(entries) == 0
	}, "batch goroutine to settle")
	// Scans are consumed by the commit: once the batch has settled, a second
	// commit of the same upload must 410 instead of silently restoring twice.
	w2 := postJSON(t, r, "/apps/app1/volume-backups/upload-commit", map[string]any{
		"pairs": []map[string]any{{"archive_id": it1.ArchiveID, "specs": []string{"/opt/a:/a"}}},
	})
	if w2.Code != http.StatusGone {
		t.Fatalf("recommit code=%d body=%s, want 410", w2.Code, w2.Body.String())
	}
}

func TestCommitSnapshotSubset(t *testing.T) {
	r, e, store, vols, _ := newUploadRestoreTestEnv(t, "linux", true)
	putTestApp(t, store, []string{"/opt/a:/a", "/opt/b:/b"})
	snapshot := writeTestSnapshotTar(t, &SnapshotManifest{
		Version: "1.0", AppID: "app1", ArchiveID: "vbk_src",
		Volumes: []VolumeItemArchive{
			{VolumeSpec: "/opt/a:/a", VolumeType: "bind", HostPath: "/opt/a", ContainerPath: "/a"},
			{VolumeSpec: "/opt/b:/b", VolumeType: "bind", HostPath: "/opt/b", ContainerPath: "/b"},
		},
	})
	it := scanOneFile(t, r, snapshot, "snapshot.tar.gz")
	if it.Match != scanMatchManifest {
		t.Fatalf("match=%q, want manifest", it.Match)
	}
	// Operator keeps only the second volume of the snapshot.
	w := postJSON(t, r, "/apps/app1/volume-backups/upload-commit", map[string]any{
		"pairs": []map[string]any{{"archive_id": it.ArchiveID, "specs": []string{"/opt/b:/b"}}},
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202", w.Code, w.Body.String())
	}
	var resp struct {
		ArchiveIDs []string `json:"archive_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	arch, ok := vols.GetArchive("app1", resp.ArchiveIDs[0])
	if !ok {
		t.Fatalf("record %s not persisted", resp.ArchiveIDs[0])
	}
	// The manifest items keep their original order (the tar unpacks
	// volumes/vol_<Index> by position), with the chosen one rewritten onto
	// the current coordinates and the unchosen one left as-is.
	if len(arch.Volumes) != 2 {
		t.Fatalf("record volumes = %+v, want the full manifest list", arch.Volumes)
	}
	if arch.Volumes[0].VolumeSpec != "/opt/a:/a" || arch.Volumes[0].HostPath != "/opt/a" {
		t.Fatalf("unchosen item must keep manifest values: %+v", arch.Volumes[0])
	}
	if arch.Volumes[1].VolumeSpec != "/opt/b:/b" || arch.Volumes[1].HostPath != "/opt/b" || arch.Volumes[1].ContainerPath != "/b" {
		t.Fatalf("chosen item must carry current values: %+v", arch.Volumes[1])
	}
	if arch.VolumeSpec != "/opt/b:/b" || arch.HostPath != "/opt/b" || arch.ContainerPath != "/b" {
		t.Fatalf("record spec=%q host=%q container=%q, want single-chosen display fields", arch.VolumeSpec, arch.HostPath, arch.ContainerPath)
	}
	// Let the async batch settle before t.TempDir cleanup starts deleting
	// the staging tree out from under it.
	waitFor(t, 5*time.Second, func() bool { return !e.VolumeBusy("app1") }, "batch goroutine to settle")
}

func TestCommitVolumeUploadsValidation(t *testing.T) {
	type pair struct {
		archiveID string
		specs     []string
	}
	cases := []struct {
		name    string
		setup   func(t *testing.T, r *gin.Engine, store *Store) []pair
		wantMsg string
	}{
		{"空配对", func(*testing.T, *gin.Engine, *Store) []pair { return nil }, "请先上传并识别备份文件"},
		{"未扫描过的归档", func(*testing.T, *gin.Engine, *Store) []pair {
			return []pair{{"vbk_nosuch", []string{"/opt/a:/a"}}}
		}, "上传文件已失效"},
		{"未声明卷", func(t *testing.T, r *gin.Engine, store *Store) []pair {
			it := scanOneFile(t, r, writeTestTar(t, map[string]string{"a/x": "1"}), "a.tar")
			return []pair{{it.ArchiveID, []string{"/etc:/etc"}}}
		}, "只能恢复到应用已声明或已挂载的卷"},
		{"空 specs", func(t *testing.T, r *gin.Engine, store *Store) []pair {
			it := scanOneFile(t, r, writeTestTar(t, map[string]string{"a/x": "1"}), "a.tar")
			return []pair{{it.ArchiveID, nil}}
		}, "未选择任何目标挂载卷"},
		{"快照空 specs", func(t *testing.T, r *gin.Engine, store *Store) []pair {
			// A snapshot with no chosen volume must be rejected like any other
			// empty selection — not fall through to chosen[0] and panic.
			snap := writeTestSnapshotTar(t, &SnapshotManifest{
				Version: "1.0", AppID: "app1", ArchiveID: "vbk_src",
				Volumes: []VolumeItemArchive{{VolumeSpec: "/opt/a:/a", HostPath: "/opt/a", ContainerPath: "/a"}},
			})
			it := scanOneFile(t, r, snap, "snapshot.tar.gz")
			return []pair{{it.ArchiveID, nil}}
		}, "未选择任何目标挂载卷"},
		{"普通文件多个目标", func(t *testing.T, r *gin.Engine, store *Store) []pair {
			it := scanOneFile(t, r, writeTestTar(t, map[string]string{"a/x": "1"}), "a.tar")
			return []pair{{it.ArchiveID, []string{"/opt/a:/a", "/opt/b:/b"}}}
		}, "不是多卷快照"},
		{"同一卷两份文件", func(t *testing.T, r *gin.Engine, store *Store) []pair {
			it1 := scanOneFile(t, r, writeTestTar(t, map[string]string{"a/x": "1"}), "a.tar")
			it2 := scanOneFile(t, r, writeTestTar(t, map[string]string{"a/y": "2"}), "b.tar")
			return []pair{{it1.ArchiveID, []string{"/opt/a:/a"}}, {it2.ArchiveID, []string{"/opt/a:/a"}}}
		}, "同一挂载卷在一次导入中只能出现一次"},
		{"快照不含所选卷", func(t *testing.T, r *gin.Engine, store *Store) []pair {
			snap := writeTestSnapshotTar(t, &SnapshotManifest{
				Version: "1.0", AppID: "app1", ArchiveID: "vbk_src",
				Volumes: []VolumeItemArchive{{VolumeSpec: "/opt/a:/a", HostPath: "/opt/a", ContainerPath: "/a"}},
			})
			it := scanOneFile(t, r, snap, "snapshot.tar.gz")
			return []pair{{it.ArchiveID, []string{"/opt/b:/b"}}}
		}, "不包含挂载卷"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, e, store, _, _ := newUploadRestoreTestEnv(t, "linux", true)
			putTestApp(t, store, []string{"/opt/a:/a", "/opt/b:/b"})
			pairs := tc.setup(t, r, store)
			var raw []map[string]any
			for _, p := range pairs {
				raw = append(raw, map[string]any{"archive_id": p.archiveID, "specs": p.specs})
			}
			w := postJSON(t, r, "/apps/app1/volume-backups/upload-commit", map[string]any{"pairs": raw})
			if w.Code == http.StatusBadRequest || w.Code == http.StatusGone {
				if !strings.Contains(w.Body.String(), tc.wantMsg) {
					t.Fatalf("body=%s, want it to contain %q", w.Body.String(), tc.wantMsg)
				}
			} else {
				t.Fatalf("code=%d body=%s, want 400/410", w.Code, w.Body.String())
			}
			if e.VolumeBusy("app1") {
				t.Fatal("rejected commit must not leak the volume slot")
			}
		})
	}
}

func TestCommitRequiresPanelDomainAndLinux(t *testing.T) {
	// Scan works without a bound domain (no agent URL involved), but the
	// commit must refuse before touching any staged data.
	r, _, store, _, _ := newUploadRestoreTestEnv(t, "linux", false)
	putTestApp(t, store, []string{"/opt/a:/a"})
	it := scanOneFile(t, r, writeTestTar(t, map[string]string{"a/x": "1"}), "a.tar")
	w := postJSON(t, r, "/apps/app1/volume-backups/upload-commit", map[string]any{
		"pairs": []map[string]any{{"archive_id": it.ArchiveID, "specs": []string{"/opt/a:/a"}}},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "无法生成 Agent 下载地址") {
		t.Fatalf("code=%d body=%s, want 400 about the download URL", w.Code, w.Body.String())
	}

	// The Windows gate sits ahead of pair validation, so a commit against a
	// Windows-hosted app is rejected before any staged data is touched.
	rw, _, sw, _, _ := newUploadRestoreTestEnv(t, "windows", true)
	putTestApp(t, sw, []string{"/opt/a:/a"})
	ww := postJSON(t, rw, "/apps/app1/volume-backups/upload-commit", map[string]any{
		"pairs": []map[string]any{{"archive_id": "vbk_x", "specs": []string{"/opt/a:/a"}}},
	})
	if ww.Code != http.StatusBadRequest || !strings.Contains(ww.Body.String(), "暂仅支持 Linux 主机") {
		t.Fatalf("code=%d body=%s, want the Linux-only rejection", ww.Code, ww.Body.String())
	}
}

func TestSweepScansDropsExpired(t *testing.T) {
	us, err := NewVolumeUploadStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	old := &stagedScan{ArchiveID: "vbk_old", AppID: "app1", StagedAt: time.Now().Add(-3 * time.Hour)}
	fresh := &stagedScan{ArchiveID: "vbk_fresh", AppID: "app1", StagedAt: time.Now()}
	for _, sc := range []*stagedScan{old, fresh} {
		if _, err := us.Save(sc.ArchiveID, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
		us.PutScan(sc)
	}
	us.SweepScans()
	if _, ok := us.PeekScan("app1", "vbk_old"); ok {
		t.Fatal("expired scan must be dropped")
	}
	if _, err := os.Stat(us.Path("vbk_old")); !os.IsNotExist(err) {
		t.Fatal("expired scan file must be removed")
	}
	if _, ok := us.PeekScan("app1", "vbk_fresh"); !ok {
		t.Fatal("fresh scan must survive")
	}
}

func TestRunRestoreUploadedBatchOffline(t *testing.T) {
	// Direct engine call (no goroutine): with no registry the batch must fail
	// fast with the offline sentinel, record a failed outcome on every pair,
	// and drop the panel-staged files and tokens however it ends.
	e, store, vols := newVolumeTestEngine(t)
	us, err := NewVolumeUploadStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewVolumeUploadStore: %v", err)
	}
	e.SetVolumeUploadStore(us)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image",
		Volumes: []string{"/opt/a:/a", "/opt/b:/b"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	var items []*uploadedRestoreItem
	var tokens []string
	for i, spec := range []string{"/opt/a:/a", "/opt/b:/b"} {
		id := fmt.Sprintf("vbk_up%d", i)
		if _, err := us.Save(id, strings.NewReader("fake-tar-bytes")); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
		tok, err := us.IssueToken(app.ID, id)
		if err != nil {
			t.Fatalf("IssueToken %s: %v", id, err)
		}
		tokens = append(tokens, tok)
		arch := &VolumeArchive{ID: id, AppID: app.ID, HostID: "h1",
			VolumeSpec: spec, Status: VolumeBackupUploaded, Source: VolumeArchiveSourceUpload}
		if err := vols.AddArchive(arch); err != nil {
			t.Fatalf("AddArchive %s: %v", id, err)
		}
		items = append(items, &uploadedRestoreItem{
			Arch: arch, DownloadURL: "http://panel.example.com/volume-uploads/" + tok,
		})
	}
	if _, err := e.runRestoreUploadedBatch(app, items, RestoreVolumeOptions{CleanRestore: true}); !errors.Is(err, snapshots.ErrVolumeRestoreOffline) {
		t.Fatalf("want offline sentinel, got %v", err)
	}
	for _, it := range items {
		arch, ok := vols.GetArchive(app.ID, it.Arch.ID)
		if !ok || arch.LastRestoreStatus != VolumeRestoreFailed ||
			!strings.Contains(arch.LastRestoreError, "目标主机 Agent 不在线") {
			t.Fatalf("record %s outcome = %+v, want failed/offline", it.Arch.ID, arch)
		}
		if _, serr := os.Stat(us.Path(it.Arch.ID)); !os.IsNotExist(serr) {
			t.Fatalf("staged upload %s must be removed after the run", it.Arch.ID)
		}
	}
	for _, tok := range tokens {
		if _, _, _, _, ok := us.peekToken(tok); ok {
			t.Fatalf("token %s must be dropped after the run", tok)
		}
	}
}
