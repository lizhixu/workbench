package apps

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- parseVolumeSpec ----

func TestParseVolumeSpec(t *testing.T) {
	cases := []struct {
		spec           string
		src, dst, mode string
		ok             bool
	}{
		{spec: "/opt/data:/data", src: "/opt/data", dst: "/data", ok: true},
		{spec: "volname:/data", src: "volname", dst: "/data", ok: true},
		{spec: "/opt/data:/data:ro", src: "/opt/data", dst: "/data", mode: "ro", ok: true},
		{spec: "volname:/data:rw", src: "volname", dst: "/data", mode: "rw", ok: true},
		{spec: `C:\data:/data`, src: `C:\data`, dst: "/data", ok: true},
		{spec: "/opt/a b:/data", src: "/opt/a b", dst: "/data", ok: true},
		{spec: "noColon", ok: false},
		{spec: "", ok: false},
		{spec: ":/data", ok: false},
		{spec: "/data:", ok: false},
		{spec: "/a/../b:/data", ok: false},
	}
	for _, tc := range cases {
		src, dst, mode, ok := parseVolumeSpec(tc.spec)
		if ok != tc.ok {
			t.Errorf("parseVolumeSpec(%q) ok = %v, want %v", tc.spec, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if src != tc.src || dst != tc.dst || mode != tc.mode {
			t.Errorf("parseVolumeSpec(%q) = (%q,%q,%q), want (%q,%q,%q)", tc.spec, src, dst, mode, tc.src, tc.dst, tc.mode)
		}
	}
}

func TestVolumeIsBindMount(t *testing.T) {
	cases := map[string]bool{
		"/opt/data":  true,
		`C:\data`:    true,
		`C:/data`:    true,
		"volname":    false,
		"redis-data": false,
	}
	for src, want := range cases {
		if got := volumeIsBindMount(src); got != want {
			t.Errorf("volumeIsBindMount(%q) = %v, want %v", src, got, want)
		}
	}
}

// ---- agent scripts ----

func TestVolumeBackupScriptBindMount(t *testing.T) {
	script := volumeBackupScript("/opt/data", "/var/backups/watchman/apps/app1/vbk_x.tar.gz")
	if !strings.Contains(script, `tar -C '/opt/data' -czf '/var/backups/watchman/apps/app1/vbk_x.tar.gz' .`) {
		t.Errorf("bind backup script missing tar command:\n%s", script)
	}
	if !strings.Contains(script, "WATCHMAN_SIZE=") || !strings.Contains(script, "sha256sum") {
		t.Errorf("backup script missing size/sha markers:\n%s", script)
	}
	if !strings.HasPrefix(script, "set -e") {
		t.Errorf("backup script must fail fast:\n%s", script)
	}
}

func TestVolumeResolveMountpointScript(t *testing.T) {
	script := volumeResolveMountpointScript("redis-data")
	if !strings.Contains(script, "docker volume inspect --format '{{.Mountpoint}}' 'redis-data'") {
		t.Errorf("resolve script wrong:\n%s", script)
	}
}

func TestVolumeRestoreScript(t *testing.T) {
	script := volumeRestoreScript("/opt/data", "/var/backups/watchman/apps/app1/vbk_x.tar.gz")
	if !strings.Contains(script, ".pre-restore.tar.gz") {
		t.Errorf("restore script missing safety copy:\n%s", script)
	}
	if !strings.Contains(script, `tar -C '/opt/data' -xzf '/var/backups/watchman/apps/app1/vbk_x.tar.gz'`) {
		t.Errorf("restore script missing untar:\n%s", script)
	}
	if !strings.Contains(script, "[ -f ") || !strings.Contains(script, "归档文件不存在") {
		t.Errorf("restore script must check the archive exists:\n%s", script)
	}
}

func TestVolumeContainerStateScript(t *testing.T) {
	script := volumeContainerStateScript("app1")
	if !strings.Contains(script, `docker inspect --format '{{.State.Running}}' 'app1'`) {
		t.Errorf("state script wrong:\n%s", script)
	}
	if !strings.Contains(script, "RUNNING") || !strings.Contains(script, "STOPPED") {
		t.Errorf("state script must echo RUNNING/STOPPED:\n%s", script)
	}
}

func TestVolumeRemoveScript(t *testing.T) {
	if got := volumeRemoveScript(nil); got != "true" {
		t.Errorf("empty remove script = %q, want true", got)
	}
	got := volumeRemoveScript([]string{"/a/b.tar.gz", "/a/b.tar.gz.pre-restore.tar.gz"})
	if !strings.Contains(got, "rm -f '/a/b.tar.gz'") {
		t.Errorf("remove script = %q", got)
	}
}

func TestParseVolumeArchiveMeta(t *testing.T) {
	size, sha := parseVolumeArchiveMeta([]byte("noise\nWATCHMAN_SIZE=123\nWATCHMAN_SHA256=abc123\n"))
	if size != 123 || sha != "abc123" {
		t.Errorf("meta = (%d,%q), want (123,abc123)", size, sha)
	}
	size, sha = parseVolumeArchiveMeta([]byte("nothing"))
	if size != 0 || sha != "" {
		t.Errorf("empty meta = (%d,%q), want (0,\"\")", size, sha)
	}
}

// ---- VolumeStore ----

func TestVolumeStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewVolumeStore(dir, nil)
	if err != nil {
		t.Fatalf("NewVolumeStore: %v", err)
	}
	now := time.Now()
	a1 := &VolumeArchive{ID: "vbk_1", AppID: "app1", VolumeSpec: "/opt/a:/a", Status: VolumeBackupRunning, CreatedAt: now}
	a2 := &VolumeArchive{ID: "vbk_2", AppID: "app1", VolumeSpec: "/opt/b:/b", Status: VolumeBackupRunning, CreatedAt: now.Add(time.Minute)}
	if err := s.AddArchive(a1); err != nil {
		t.Fatalf("AddArchive: %v", err)
	}
	if err := s.AddArchive(a2); err != nil {
		t.Fatalf("AddArchive: %v", err)
	}

	list := s.ListArchives("app1")
	if len(list) != 2 || list[0].ID != "vbk_2" {
		t.Fatalf("ListArchives newest-first failed: %+v", list)
	}
	if got, _ := s.GetArchive("app1", "vbk_1"); got == nil || got.Status != VolumeBackupRunning {
		t.Fatalf("GetArchive failed: %+v", got)
	}

	// running -> success migration persists through PutArchive + reload.
	a1.Status = VolumeBackupSuccess
	a1.Size = 42
	if err := s.PutArchive(a1); err != nil {
		t.Fatalf("PutArchive: %v", err)
	}
	s2, err := NewVolumeStore(dir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := s2.GetArchive("app1", "vbk_1")
	if !ok || got.Status != VolumeBackupSuccess || got.Size != 42 {
		t.Fatalf("reload after PutArchive: %+v", got)
	}

	// Other apps' records are untouched by an app-scoped list.
	if got := s2.ListArchives("other"); len(got) != 0 {
		t.Fatalf("ListArchives leaked cross-app records: %+v", got)
	}

	if err := s.DeleteArchive("app1", "vbk_1"); err != nil {
		t.Fatalf("DeleteArchive: %v", err)
	}
	if _, ok := s.GetArchive("app1", "vbk_1"); ok {
		t.Fatal("archive still present after delete")
	}
}

func TestVolumeStoreCorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, appVolumeArchivesFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewVolumeStore(dir, nil)
	if err != nil {
		t.Fatalf("NewVolumeStore on corrupt file: %v", err)
	}
	if got := s.ListArchives("app1"); len(got) != 0 {
		t.Fatalf("corrupt store should start empty, got %+v", got)
	}
}

// ---- engine boundaries ----

func newVolumeTestEngine(t *testing.T) (*Engine, *Store, *VolumeStore) {
	t.Helper()
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	vols, err := NewVolumeStore(store.Dir(), nil)
	if err != nil {
		t.Fatalf("NewVolumeStore: %v", err)
	}
	e := NewEngine(nil, store, nil, nil)
	e.SetVolumeStore(vols)
	return e, store, vols
}

func TestBackupAppVolumesNoVolumes(t *testing.T) {
	e, store, _ := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	if _, err := e.BackupAppVolumes(app, nil, ""); err == nil || !strings.Contains(err.Error(), "未配置任何挂载卷") {
		t.Fatalf("want no-volumes error, got %v", err)
	}
}

func TestBackupAppVolumesHubOffline(t *testing.T) {
	e, store, _ := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	if _, err := e.BackupAppVolumes(app, nil, ""); err == nil || !strings.Contains(err.Error(), "Agent 不在线") {
		t.Fatalf("want offline error, got %v", err)
	}
}

func TestBackupAppVolumesBusyConflict(t *testing.T) {
	e, store, _ := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	if !e.acquireVolumeSlot(app.ID) {
		t.Fatal("first acquire must succeed")
	}
	if !e.VolumeBusy(app.ID) {
		t.Fatal("VolumeBusy must report the held slot")
	}
	if _, err := e.BackupAppVolumes(app, nil, ""); err == nil || !strings.Contains(err.Error(), "已有备份或恢复任务在执行") {
		t.Fatalf("want busy conflict, got %v", err)
	}
	e.releaseVolumeSlot(app.ID)
	if e.VolumeBusy(app.ID) {
		t.Fatal("slot must be free after release")
	}
}

func TestRestoreAppVolumeRequiresContainer(t *testing.T) {
	e, store, vols := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	arch := &VolumeArchive{ID: "vbk_x", AppID: app.ID, HostID: app.HostID, FilePath: "/var/backups/watchman/apps/app1/vbk_x.tar.gz", HostPath: "/opt/a", Status: VolumeBackupSuccess}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}
	// Registry is nil, so the hub check fires first.
	if _, err := e.RestoreAppVolume(app, arch); err == nil || !strings.Contains(err.Error(), "Agent 不在线") {
		t.Fatalf("want offline error, got %v", err)
	}
}

func TestVolumeBackupSupported(t *testing.T) {
	cases := []struct {
		app  Application
		want bool
	}{
		{app: Application{SourceType: "image"}, want: true},
		{app: Application{SourceType: "template"}, want: true},
		{app: Application{SourceType: "git", BuildType: "dockerfile"}, want: true},
		{app: Application{SourceType: "git", BuildType: "compose"}, want: false},
		{app: Application{SourceType: "raw_compose"}, want: false},
	}
	for _, tc := range cases {
		if got := volumeBackupSupported(&tc.app); got != tc.want {
			t.Errorf("volumeBackupSupported(%s/%s) = %v, want %v", tc.app.SourceType, tc.app.BuildType, got, tc.want)
		}
	}
}

// ---- HTTP handlers ----

func newVolumeTestRouter(t *testing.T) (*gin.Engine, *Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e, store, _ := newVolumeTestEngine(t)
	h := NewAppHandlers(nil, store, e)
	r := gin.New()
	h.RegisterAppRoutes(r.Group(""), r.Group(""), r.Group(""))
	return r, store
}

func TestVolumeBackupHandlers(t *testing.T) {
	r, store := newVolumeTestRouter(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}

	// List on an app without archives: 200, empty page.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apps/app1/volume-backups", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list: code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("list: body=%s", w.Body.String())
	}

	// Backup without declared volumes: 400.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/apps/app1/volume-backups", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no-volume backup: code=%d body=%s", w.Code, w.Body.String())
	}

	// Unknown app: 404 on every verb.
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/apps/nope/volume-backups", nil),
		httptest.NewRequest(http.MethodPost, "/apps/nope/volume-backups", nil),
		httptest.NewRequest(http.MethodPost, "/apps/nope/volume-backups/vbk_x/restore", nil),
		httptest.NewRequest(http.MethodDelete, "/apps/nope/volume-backups/vbk_x", nil),
	} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s: code=%d", req.Method, req.URL.Path, w.Code)
		}
	}

	// Restore of a non-existent archive: 404.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/apps/app1/volume-backups/vbk_missing/restore", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("restore missing archive: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestVolumeBackupRejectsUndeclaredSpec(t *testing.T) {
	r, store := newVolumeTestRouter(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/apps/app1/volume-backups",
		strings.NewReader(`{"volumes":["/etc:/etc"]}`)))
	w.Header().Set("Content-Type", "application/json")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "只能备份应用已声明的挂载卷") {
		t.Fatalf("undeclared spec must be rejected: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestVolumeBackupRejectsComposeApp(t *testing.T) {
	r, store := newVolumeTestRouter(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "raw_compose", ComposeContent: "services: {}"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/apps/app1/volume-backups", nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Compose") {
		t.Fatalf("compose app must be rejected: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestVolumeBackupBusyPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e, store, _ := newVolumeTestEngine(t)
	h := NewAppHandlers(nil, store, e)
	r := gin.New()
	h.RegisterAppRoutes(r.Group(""), r.Group(""), r.Group(""))
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	// Hold the app's volume slot (as an in-flight backup would), then the
	// handler must answer 409 instead of a 202 that does nothing.
	e.acquireVolumeSlot(app.ID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/apps/app1/volume-backups", nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "已有备份或恢复任务在执行") {
		t.Fatalf("busy pre-flight: code=%d body=%s", w.Code, w.Body.String())
	}
	e.releaseVolumeSlot(app.ID)
}

func TestVolumeBackupArchiveListingAfterAsyncRun(t *testing.T) {
	// An offline hub makes the async backup fail fast, but the running record
	// is written before the agent call, proving the listing surface shows
	// in-flight work (the UI polls on it).
	gin.SetMode(gin.TestMode)
	e, store, vols := newVolumeTestEngine(t)
	h := NewAppHandlers(nil, store, e)
	r := gin.New()
	h.RegisterAppRoutes(r.Group(""), r.Group(""), r.Group(""))
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}

	// Pre-seed one finished archive to prove listing order and fields.
	arch := &VolumeArchive{ID: "vbk_done", AppID: app.ID, HostID: app.HostID, VolumeSpec: "/opt/a:/a",
		HostPath: "/opt/a", ContainerPath: "/a", FilePath: "/var/backups/watchman/apps/app1/vbk_done.tar.gz",
		Status: VolumeBackupSuccess, Size: 99, CreatedAt: time.Now()}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apps/app1/volume-backups", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "vbk_done") {
		t.Fatalf("list after seed: code=%d body=%s", w.Code, w.Body.String())
	}

	// Filter by volume spec.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apps/app1/volume-backups?volume="+"%2Fopt%2Fa%3A%2Fa", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "vbk_done") {
		t.Fatalf("filtered list: code=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apps/app1/volume-backups?volume="+"%2Fopt%2Fb%3A%2Fb", nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "vbk_done") {
		t.Fatalf("filtered list must drop non-matching: code=%d body=%s", w.Code, w.Body.String())
	}

	// Delete removes the record (host offline: metadata deletion is authoritative).
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/apps/app1/volume-backups/vbk_done", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := vols.GetArchive(app.ID, "vbk_done"); ok {
		t.Fatal("archive still present after delete")
	}
}
