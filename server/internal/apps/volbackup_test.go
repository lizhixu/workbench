package apps

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/snapshots"
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
		{spec: "/opt/data:data", ok: false},    // container path must be absolute
		{spec: "volname:data", ok: false},      // same for named volumes
		{spec: "/opt/data:/data:z", ok: false}, // unknown third segment is not a mode
		{spec: "/opt/data:/data:ro:rw", ok: false},
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
	script := volumeRestoreScript("/opt/data", "/var/backups/watchman/apps/app1/vbk_x.tar.gz",
		"/var/backups/watchman/apps/app1/vbk_x.pre-restore.tar.gz")
	if !strings.Contains(script, "/var/backups/watchman/apps/app1/vbk_x.pre-restore.tar.gz") {
		t.Errorf("restore script missing safety copy:\n%s", script)
	}
	if !strings.Contains(script, `tar -C '/opt/data' -xzf '/var/backups/watchman/apps/app1/vbk_x.tar.gz'`) {
		t.Errorf("restore script missing untar:\n%s", script)
	}
	if !strings.Contains(script, "[ -f ") || !strings.Contains(script, "归档文件不存在") {
		t.Errorf("restore script must check the archive exists:\n%s", script)
	}
}

func TestVolumeUploadScript(t *testing.T) {
	script := volumeUploadScript("/var/backups/watchman/apps/app1/vbk_x.tar.gz",
		"https://s3.example.com/bucket/apps/app1/vbk_x.tar.gz?X-Amz-Signature=abc")
	if !strings.Contains(script, "curl -fsSL") || !strings.Contains(script, "-X PUT -T '/var/backups/watchman/apps/app1/vbk_x.tar.gz'") {
		t.Errorf("upload script must PUT the staged archive:\n%s", script)
	}
	if !strings.Contains(script, "'https://s3.example.com/bucket/apps/app1/vbk_x.tar.gz?X-Amz-Signature=abc'") {
		t.Errorf("upload script must carry the pre-signed URL intact:\n%s", script)
	}
}

func TestVolumeDownloadScript(t *testing.T) {
	script := volumeDownloadScript("https://s3.example.com/bucket/apps/app1/vbk_x.tar.gz?X-Amz-Signature=abc",
		"/var/backups/watchman/apps/app1/vbk_x.restore.tar.gz")
	if !strings.Contains(script, "curl -fsSL") || !strings.Contains(script, "-o '/var/backups/watchman/apps/app1/vbk_x.restore.tar.gz'") {
		t.Errorf("download script must write the archive to the staging path:\n%s", script)
	}
	if !strings.Contains(script, "[ -s ") || !strings.Contains(script, "归档下载为空") {
		t.Errorf("download script must reject an empty body:\n%s", script)
	}
}

func TestVolumeContainerStateScript(t *testing.T) {
	script := volumeContainerStateScript("app1")
	if !strings.Contains(script, `docker inspect --format '{{.State.Running}}' 'app1'`) {
		t.Errorf("state script wrong:\n%s", script)
	}
	if !strings.Contains(script, "set -e") || !strings.Contains(script, "RUNNING") || !strings.Contains(script, "STOPPED") {
		t.Errorf("state script must fail closed and echo RUNNING/STOPPED:\n%s", script)
	}
}

func TestParseVolumeContainerStateFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{output: "RUNNING", want: true},
		{output: "STOPPED", want: false},
		{output: "", want: false},
		{output: "false\\nerror", want: false},
		{output: "unknown", want: false},
	} {
		got, err := parseVolumeContainerState([]byte(tc.output))
		if tc.output == "RUNNING" || tc.output == "STOPPED" {
			if err != nil || got != tc.want {
				t.Errorf("parseVolumeContainerState(%q) = (%v,%v), want (%v,nil)", tc.output, got, err, tc.want)
			}
		} else if err == nil {
			t.Errorf("parseVolumeContainerState(%q) accepted invalid state", tc.output)
		}
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

	// Records are deep copies: HTTP handlers marshal them while the engine
	// mutates status/size, so a shared pointer would be a data race.
	got.Status = VolumeBackupFailed
	got.LastRestoreAt = &now
	if again, _ := s2.GetArchive("app1", "vbk_1"); again.Status != VolumeBackupSuccess || again.LastRestoreAt != nil {
		t.Fatalf("GetArchive leaked the stored pointer: %+v", again)
	}
	listed := s2.ListArchives("app1")
	listed[0].Status = "mutated"
	if again, _ := s2.GetArchive("app1", listed[0].ID); again.Status == "mutated" {
		t.Fatal("ListArchives leaked the stored pointer")
	}

	// Other apps' records are untouched by an app-scoped list.
	if got := s2.ListArchives("other"); len(got) != 0 {
		t.Fatalf("ListArchives leaked cross-app records: %+v", got)
	}

	if err := s2.DeleteArchive("app1", "vbk_1"); err != nil {
		t.Fatalf("DeleteArchive: %v", err)
	}
	if _, ok := s2.GetArchive("app1", "vbk_1"); ok {
		t.Fatal("archive still present after delete")
	}

	if err := s2.DeleteAppArchives("app1"); err != nil {
		t.Fatalf("DeleteAppArchives: %v", err)
	}
	if len(s2.ListArchives("app1")) != 0 {
		t.Fatal("archives still present after DeleteAppArchives")
	}
}

func TestVolumeStoreReconcilesInterruptedRunning(t *testing.T) {
	dir := t.TempDir()
	doc := `{"archives":[{"id":"vbk_1","app_id":"app1","status":"running","created_at":"2026-10-22T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, appVolumeArchivesFile), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewVolumeStore(dir, nil)
	if err != nil {
		t.Fatalf("NewVolumeStore: %v", err)
	}
	got, ok := s.GetArchive("app1", "vbk_1")
	if !ok || got.Status != VolumeBackupFailed || !strings.Contains(got.Error, "中断") {
		t.Fatalf("running record not reconciled: %+v", got)
	}
	// The reconciliation is persisted, so a second restart does not flip it again.
	s2, err := NewVolumeStore(dir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, _ := s2.GetArchive("app1", "vbk_1"); got.Status != VolumeBackupFailed {
		t.Fatalf("reconciled status not persisted: %+v", got)
	}
}

func TestVolumeStoreReconcilesInterruptedUploadedRestore(t *testing.T) {
	dir := t.TempDir()
	doc := `{"archives":[{"id":"vbk_up1","app_id":"app1","status":"uploaded","source":"upload","created_at":"2026-10-22T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, appVolumeArchivesFile), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewVolumeStore(dir, nil)
	if err != nil {
		t.Fatalf("NewVolumeStore: %v", err)
	}
	got, ok := s.GetArchive("app1", "vbk_up1")
	if !ok || got.LastRestoreStatus != VolumeBackupFailed || !strings.Contains(got.LastRestoreError, "中断") {
		t.Fatalf("interrupted upload record not reconciled: %+v", got)
	}
	s2, err := NewVolumeStore(dir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got2, _ := s2.GetArchive("app1", "vbk_up1"); got2.LastRestoreStatus != VolumeBackupFailed {
		t.Fatalf("reconciled status not persisted: %+v", got2)
	}
}

func TestVolumeStoreTransactionalOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := NewVolumeStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	arch1 := &VolumeArchive{ID: "vbk_1", AppID: "app1", VolumeSpec: "/data:/data", Status: VolumeBackupSuccess, CreatedAt: time.Now()}
	if err := s.AddArchive(arch1); err != nil {
		t.Fatal(err)
	}
	// Point to an inaccessible nested path so Mkdir/write fails
	s.dir = filepath.Join(dir, "invalid-file")
	if err := os.WriteFile(s.dir, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	arch2 := &VolumeArchive{ID: "vbk_2", AppID: "app1", VolumeSpec: "/data:/data", Status: VolumeBackupSuccess, CreatedAt: time.Now()}
	if err := s.AddArchive(arch2); err == nil {
		t.Fatal("expected save error on invalid directory")
	}
	// In-memory store must not retain arch2
	if _, ok := s.GetArchive("app1", "vbk_2"); ok {
		t.Fatal("unpersisted archive retained in memory after save failure")
	}
	if _, ok := s.GetArchive("app1", "vbk_1"); !ok {
		t.Fatal("existing archive lost from memory after failed save")
	}
}

func TestStaleVolumeArchivesCountsOnlySuccess(t *testing.T) {
	now := time.Now()
	all := make([]*VolumeArchive, 0, 10)
	for i := 0; i < 7; i++ {
		all = append(all, &VolumeArchive{
			ID: fmt.Sprintf("ok%d", i), VolumeSpec: "/a:/a", Status: VolumeBackupSuccess,
			CreatedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}
	all = append(all,
		&VolumeArchive{ID: "failed", VolumeSpec: "/a:/a", Status: VolumeBackupFailed, CreatedAt: now.Add(-time.Hour)},
		&VolumeArchive{ID: "other", VolumeSpec: "/b:/b", Status: VolumeBackupSuccess, CreatedAt: now.Add(-time.Hour)},
	)
	stale := staleVolumeArchives(all, "/a:/a", volumeRetention)
	if len(stale) != 2 || stale[0].ID != "ok5" || stale[1].ID != "ok6" {
		t.Fatalf("stale = %+v, want [ok5 ok6] (failed records must not consume retention)", stale)
	}
	if got := staleVolumeArchives(all, "/b:/b", volumeRetention); len(got) != 0 {
		t.Fatalf("other spec must keep its only archive, got %+v", got)
	}
	// A snapshot job's retention count overrides the package default.
	if got := staleVolumeArchives(all, "/a:/a", 2); len(got) != 5 {
		t.Fatalf("keep=2 stale = %d records, want 5", len(got))
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

// fakeS3TargetStore implements S3TargetStore for tests. The target points at an
// httptest server so delete calls have somewhere to go (204).
type fakeS3TargetStore struct {
	def     *snapshots.S3Target
	targets map[string]*snapshots.S3Target
}

func (f *fakeS3TargetStore) GetDefaultS3Target() (*snapshots.S3Target, bool) {
	if f == nil || f.def == nil {
		return nil, false
	}
	return f.def, true
}

func (f *fakeS3TargetStore) GetS3Target(id string) (*snapshots.S3Target, bool) {
	if f == nil {
		return nil, false
	}
	t, ok := f.targets[id]
	return t, ok
}

func newFakeS3TargetStore(t *testing.T) *fakeS3TargetStore {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	target := &snapshots.S3Target{
		ID: "s3_test", Name: "测试存储", Endpoint: srv.URL, Region: "auto",
		Bucket: "bucket", AccessKey: "ak", SecretKey: "sk", IsDefault: true,
	}
	return &fakeS3TargetStore{def: target, targets: map[string]*snapshots.S3Target{target.ID: target}}
}

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
	e.SetS3TargetStore(newFakeS3TargetStore(t))
	return e, store, vols
}

func TestBackupAppVolumesNoS3Target(t *testing.T) {
	e, store, _ := newVolumeTestEngine(t)
	// Simulate an unconfigured snapshot module (no default S3 target).
	e.SetS3TargetStore(&fakeS3TargetStore{})
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	if _, err := e.BackupAppVolumes(app, nil, ""); err == nil || !strings.Contains(err.Error(), "未配置默认 S3 存储目标") {
		t.Fatalf("want missing-S3 error, got %v", err)
	}
	if e.VolumeBusy(app.ID) {
		t.Fatal("rejected backup must not leak the volume slot")
	}
	if e.S3Status().Ready {
		t.Fatal("S3Status must report unready without a default target")
	}
}

func TestS3StatusReportsDefaultTarget(t *testing.T) {
	e, _, _ := newVolumeTestEngine(t)
	got := e.S3Status()
	if !got.Ready || got.TargetID != "s3_test" || got.TargetName != "测试存储" || got.Bucket != "bucket" {
		t.Fatalf("S3Status = %+v", got)
	}
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

func TestRestoreAppVolumeOfflineFirst(t *testing.T) {
	e, store, vols := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	arch := &VolumeArchive{ID: "vbk_x", AppID: app.ID, HostID: app.HostID,
		StorageTarget: "s3_test", StoragePath: "apps/app1/vbk_x.tar.gz", HostPath: "/opt/a", Status: VolumeBackupSuccess}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}
	// Registry is nil (host offline), so the hub check fires before the
	// container-name guard; the restore must not touch the archive record.
	if _, err := e.RestoreAppVolume(app, arch); err == nil || !strings.Contains(err.Error(), "Agent 不在线") {
		t.Fatalf("want offline error, got %v", err)
	}
	got, ok := vols.GetArchive(app.ID, arch.ID)
	if !ok || got.LastRestoreStatus != VolumeRestoreFailed || !strings.Contains(got.LastRestoreError, "Agent 不在线") {
		t.Fatalf("offline restore outcome not recorded: %+v", got)
	}
}

func TestCheckAppVolumeRestoreSentinels(t *testing.T) {
	e, store, vols := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	arch := &VolumeArchive{ID: "vbk_x", AppID: app.ID, HostID: app.HostID,
		StorageTarget: "s3_test", StoragePath: "apps/app1/vbk_x.tar.gz", HostPath: "/opt/a", Status: VolumeBackupSuccess}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}
	// Registry nil -> offline sentinel, reported before anything runs.
	if err := e.CheckAppVolumeRestore(app.ID); !errors.Is(err, snapshots.ErrVolumeRestoreOffline) {
		t.Fatalf("want offline sentinel, got %v", err)
	}
	if err := e.CheckAppVolumeRestore("nope"); err == nil {
		t.Fatal("missing app must be rejected")
	}
	// Busy slot -> busy sentinel.
	e.acquireVolumeSlot(app.ID)
	if err := e.CheckAppVolumeRestore(app.ID); !errors.Is(err, snapshots.ErrVolumeRestoreBusy) {
		t.Fatalf("want busy sentinel, got %v", err)
	}
	e.releaseVolumeSlot(app.ID)
	// Deployment in flight -> deploying sentinel.
	if err := e.registerRunning(app.ID, &activeDeployment{depID: "dep_x", cancel: func() {}}); err != nil {
		t.Fatal(err)
	}
	defer e.finishRunning(app.ID, "dep_x")
	if err := e.CheckAppVolumeRestore(app.ID); !errors.Is(err, snapshots.ErrVolumeRestoreDeploying) {
		t.Fatalf("want deploying sentinel, got %v", err)
	}
}

func TestRestoreAppVolumeByIDRecordsPreflightFailure(t *testing.T) {
	e, store, vols := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	arch := &VolumeArchive{ID: "vbk_x", AppID: app.ID, HostID: app.HostID,
		StorageTarget: "s3_test", StoragePath: "apps/app1/vbk_x.tar.gz", HostPath: "/opt/a", Status: VolumeBackupSuccess}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}
	// Simulate the race the HTTP layer cannot see: its synchronous preflight
	// passed, then a backup claimed the slot before the goroutine ran.
	e.acquireVolumeSlot(app.ID)
	if err := e.RestoreAppVolumeByID(app.ID, arch.ID); !errors.Is(err, snapshots.ErrVolumeRestoreBusy) {
		t.Fatalf("want busy sentinel, got %v", err)
	}
	e.releaseVolumeSlot(app.ID)
	got, ok := vols.GetArchive(app.ID, arch.ID)
	if !ok || got.LastRestoreStatus != VolumeRestoreFailed || got.LastRestoreAt == nil {
		t.Fatalf("preflight rejection must be recorded on the archive: %+v", got)
	}
	if !strings.Contains(got.LastRestoreError, "已有备份或恢复任务在执行") {
		t.Fatalf("recorded error must explain the rejection: %q", got.LastRestoreError)
	}
}

func TestDeleteAppVolumeArchiveBusy(t *testing.T) {
	e, store, vols := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	arch := &VolumeArchive{ID: "vbk_x", AppID: app.ID, HostID: app.HostID,
		StorageTarget: "s3_test", StoragePath: "apps/app1/vbk_x.tar.gz", HostPath: "/opt/a", Status: VolumeBackupSuccess}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}
	e.acquireVolumeSlot(app.ID)
	if err := e.DeleteAppVolumeArchive(app, arch); err == nil || !strings.Contains(err.Error(), "已有备份或恢复任务在执行") {
		t.Fatalf("want busy conflict, got %v", err)
	}
	e.releaseVolumeSlot(app.ID)
	// Host offline: metadata deletion is authoritative and must go through.
	if err := e.DeleteAppVolumeArchive(app, arch); err != nil {
		t.Fatalf("DeleteAppVolumeArchive: %v", err)
	}
	if _, ok := vols.GetArchive(app.ID, arch.ID); ok {
		t.Fatal("archive still present after delete")
	}
}

func TestDeploymentRejectedWhileVolumeBusy(t *testing.T) {
	e, store, _ := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	e.acquireVolumeSlot(app.ID)
	defer e.releaseVolumeSlot(app.ID)
	if _, err := e.StartDeployment(app, "manual", "tester"); !errors.Is(err, errVolumeOpInProgress) {
		t.Fatalf("want volume conflict, got %v", err)
	}
	if e.IsRunning(app.ID) {
		t.Fatal("rejected deployment must not leak the running slot")
	}
}

func TestVolumeBackupRejectedWhileDeploying(t *testing.T) {
	e, store, _ := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	if err := e.registerRunning(app.ID, &activeDeployment{depID: "dep_x", cancel: func() {}}); err != nil {
		t.Fatal(err)
	}
	defer e.finishRunning(app.ID, "dep_x")
	if _, err := e.BackupAppVolumes(app, nil, ""); err == nil || !strings.Contains(err.Error(), "正在部署") {
		t.Fatalf("want deploying conflict, got %v", err)
	}
	// The wrapper releases its own volume slot on the early return.
	if e.VolumeBusy(app.ID) {
		t.Fatal("rejected backup must not leak the volume slot")
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

func newVolumeTestRouter(t *testing.T) (*gin.Engine, *Store, *VolumeStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e, store, vols := newVolumeTestEngine(t)
	h := NewAppHandlers(nil, store, e)
	r := gin.New()
	h.RegisterAppRoutes(r.Group(""), r.Group(""), r.Group(""))
	return r, store, vols
}

func TestVolumeBackupHandlers(t *testing.T) {
	r, store, _ := newVolumeTestRouter(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}

	// List on an app without archives: 200, empty page, S3 destination exposed.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apps/app1/volume-backups", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list: code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"total":0`) || !strings.Contains(w.Body.String(), `"s3":{"ready":true`) {
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
	r, store, _ := newVolumeTestRouter(t)
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
	r, store, _ := newVolumeTestRouter(t)
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

func TestVolumeBackupListingAndDelete(t *testing.T) {
	// Listing surface: order, field passthrough, per-volume filter, and delete
	// (host offline -> metadata deletion is authoritative).
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
		HostPath: "/opt/a", ContainerPath: "/a",
		StorageTarget: "s3_test", StorageTargetName: "测试存储", StoragePath: "apps/app1/vbk_done.tar.gz",
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

func TestVolumeRestoreRejectsUnsuccessfulArchive(t *testing.T) {
	r, store, vols := newVolumeTestRouter(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", ContainerName: "app1"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{VolumeBackupFailed, VolumeBackupRunning} {
		arch := &VolumeArchive{ID: "vbk_" + status, AppID: app.ID, HostID: app.HostID,
			StorageTarget: "s3_test", StoragePath: "apps/app1/x.tar.gz", HostPath: "/opt/a", Status: status}
		if err := vols.AddArchive(arch); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/apps/app1/volume-backups/"+arch.ID+"/restore", nil))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不能用于恢复") {
			t.Fatalf("restore %s archive: code=%d body=%s", status, w.Code, w.Body.String())
		}
	}
}

func TestVolumeArchiveDownloadRedirect(t *testing.T) {
	r, store, vols := newVolumeTestRouter(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	arch := &VolumeArchive{ID: "vbk_dl", AppID: app.ID, HostID: app.HostID, VolumeSpec: "/opt/a:/a",
		HostPath: "/opt/a", StorageTarget: "s3_test", StoragePath: "apps/app1/vbk_dl.tar.gz",
		Status: VolumeBackupSuccess, Size: 1, CreatedAt: time.Now()}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apps/app1/volume-backups/vbk_dl/download", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("download: code=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	for _, want := range []string{"/bucket/apps/app1/vbk_dl.tar.gz", "X-Amz-Signature=", "X-Amz-Expires=900"} {
		if !strings.Contains(loc, want) {
			t.Fatalf("download redirect missing %q: %s", want, loc)
		}
	}

	// An archive that never finished has no downloadable object: 400.
	arch.Status = VolumeBackupFailed
	arch.Error = "boom"
	if err := vols.PutArchive(arch); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apps/app1/volume-backups/vbk_dl/download", nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "无法下载") {
		t.Fatalf("download failed archive: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestVolumeArchiveDownloadGoneTarget(t *testing.T) {
	r, store, vols := newVolumeTestRouter(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	// Storage target was deleted from the snapshot module after the backup.
	arch := &VolumeArchive{ID: "vbk_gone", AppID: app.ID, HostID: app.HostID, VolumeSpec: "/opt/a:/a",
		StorageTarget: "s3_deleted", StoragePath: "apps/app1/vbk_gone.tar.gz",
		Status: VolumeBackupSuccess, CreatedAt: time.Now()}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apps/app1/volume-backups/vbk_gone/download", nil))
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "存储目标已不存在") {
		t.Fatalf("download with gone target: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestVolumeBackupRejectsWithoutS3Target(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e, store, _ := newVolumeTestEngine(t)
	e.SetS3TargetStore(&fakeS3TargetStore{})
	h := NewAppHandlers(nil, store, e)
	r := gin.New()
	h.RegisterAppRoutes(r.Group(""), r.Group(""), r.Group(""))
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/apps/app1/volume-backups", nil))
	// No target chosen and no default configured: the destination-selection
	// message (2026-10-26) replaces the old default-only wording; the
	// rejection itself (400, no slot leak) is unchanged.
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "请先选择备份位置") {
		t.Fatalf("no-S3 backup: code=%d body=%s", w.Code, w.Body.String())
	}
	if e.VolumeBusy(app.ID) {
		t.Fatal("rejected backup must not leak the volume slot")
	}
}

func TestVolumeDeleteRunningArchiveConflict(t *testing.T) {
	r, store, vols := newVolumeTestRouter(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	arch := &VolumeArchive{ID: "vbk_running", AppID: app.ID, HostID: app.HostID, Status: VolumeBackupRunning}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/apps/app1/volume-backups/vbk_running", nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "执行中") {
		t.Fatalf("delete running archive: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestVolumeBackupNoteLengthLimit(t *testing.T) {
	r, store, _ := newVolumeTestRouter(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image", Volumes: []string{"/opt/a:/a"}}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	body := `{"note":"` + strings.Repeat("长", 201) + `"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/apps/app1/volume-backups", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "200") {
		t.Fatalf("over-long note: code=%d body=%s", w.Code, w.Body.String())
	}
}

// fakeSnapshotJobStore records which app's scheduled backup jobs were dropped.
type fakeSnapshotJobStore struct {
	deleted []string
	err     error
}

func (f *fakeSnapshotJobStore) DeleteJobsByApp(appID string) (int, error) {
	f.deleted = append(f.deleted, appID)
	if f.err != nil {
		return 0, f.err
	}
	return 1, nil
}

func TestPurgeVolumeArchivesDropsSnapshotJobs(t *testing.T) {
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}

	// Volume store deliberately unwired: the archives may already be gone while
	// the app's snapshot jobs still exist, and those must not outlive the app.
	jobs := &fakeSnapshotJobStore{}
	e := NewEngine(nil, store, nil, nil)
	e.SetSnapshotJobStore(jobs)
	if err := e.PurgeVolumeArchives(app); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(jobs.deleted) != 1 || jobs.deleted[0] != app.ID {
		t.Fatalf("snapshot job cleanup = %v, want [app1]", jobs.deleted)
	}

	// Unwired cleaner (unit tests, store-less engines) must stay harmless.
	if err := NewEngine(nil, store, nil, nil).PurgeVolumeArchives(app); err != nil {
		t.Fatalf("purge without cleanup hook: %v", err)
	}
}

// A failing job-store save must not skip archive cleanup: the application is
// deleted either way, and bailing out would leave paid S3 objects behind
// metadata nobody can reach anymore.
func TestPurgeVolumeArchivesStillCleansArchivesWhenJobCleanupFails(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	vols, err := NewVolumeStore(dir, nil)
	if err != nil {
		t.Fatalf("NewVolumeStore: %v", err)
	}
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := vols.AddArchive(&VolumeArchive{
		AppID: app.ID, ID: "vbk_1", VolumeSpec: "/opt/a:/a",
		Status: VolumeBackupSuccess, StorageTarget: "default", StoragePath: "apps/app1/vbk_1.tar.gz",
	}); err != nil {
		t.Fatalf("AddArchive: %v", err)
	}

	e := NewEngine(nil, store, nil, nil)
	e.SetVolumeStore(vols)
	e.SetSnapshotJobStore(&fakeSnapshotJobStore{err: errors.New("disk full")})

	err = e.PurgeVolumeArchives(app)
	if err == nil || !strings.Contains(err.Error(), "删除应用数据卷备份任务失败") {
		t.Fatalf("purge must surface the job-cleanup failure, got %v", err)
	}
	// The missing S3 target is tolerated (object unreachable), and the
	// metadata must still be dropped: no unreachable records may survive.
	if got := vols.ListArchives(app.ID); len(got) != 0 {
		t.Fatalf("archives must be dropped despite job-cleanup failure, left %d", len(got))
	}
}

// ---- restore volume-spec validation (P1-1) ----

func TestResolveRestoreVolumeDir(t *testing.T) {
	// Bind mounts resolve without touching the agent, so a nil hub is fine
	// here; named-volume re-resolution needs a live hub and is exercised
	// through the same docker volume inspect call backupAppVolume uses.
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image",
		Volumes: []string{"/opt/a:/a", "/opt/ro:/ro:ro"}}
	cases := []struct {
		name string
		arch *VolumeArchive
		want string
		err  string
	}{
		{"bind match", &VolumeArchive{VolumeSpec: "/opt/a:/a", HostPath: "/opt/a", ContainerPath: "/a"}, "/opt/a", ""},
		{"bind spec trimmed", &VolumeArchive{VolumeSpec: "  /opt/a:/a  ", HostPath: "/opt/a", ContainerPath: "/a"}, "/opt/a", ""},
		{"bind ro match", &VolumeArchive{VolumeSpec: "/opt/ro:/ro:ro", HostPath: "/opt/ro", ContainerPath: "/ro"}, "/opt/ro", ""},
		{"spec not declared", &VolumeArchive{VolumeSpec: "/opt/gone:/a", HostPath: "/opt/gone", ContainerPath: "/a"}, "", "卷配置已变更"},
		{"bind host path re-pointed", &VolumeArchive{VolumeSpec: "/opt/a:/a", HostPath: "/opt/old", ContainerPath: "/a"}, "", "卷配置已变更"},
		{"container path changed", &VolumeArchive{VolumeSpec: "/opt/a:/a", HostPath: "/opt/a", ContainerPath: "/other"}, "", "卷配置已变更"},
		{"empty spec", &VolumeArchive{VolumeSpec: "", HostPath: "/opt/a", ContainerPath: "/a"}, "", "卷规格缺失"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewEngine(nil, nil, nil, nil).resolveRestoreVolumeDir(nil, app, tc.arch)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("want dir %q, got error %v", tc.want, err)
				}
				if got != tc.want {
					t.Fatalf("dir = %q, want %q", got, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("want error containing %q, got %v", tc.err, err)
			}
		})
	}
	// A declared but malformed spec must fail parsing, not match by accident.
	badApp := &Application{ID: "app2", HostID: "h1", SourceType: "image", Volumes: []string{"a:b"}}
	if _, err := NewEngine(nil, nil, nil, nil).resolveRestoreVolumeDir(nil, badApp,
		&VolumeArchive{VolumeSpec: "a:b", HostPath: "a", ContainerPath: "b"}); err == nil || !strings.Contains(err.Error(), "卷规格无效") {
		t.Fatalf("want invalid-spec error, got %v", err)
	}
}

// ---- backup marker validation (P1-2) ----

func TestValidateVolumeArchiveMeta(t *testing.T) {
	cases := []struct {
		size int64
		sha  string
		ok   bool
	}{
		{1024, "abc", true},
		{0, "abc", false},
		{-1, "abc", false},
		{1024, "", false},
		{0, "", false},
	}
	for _, tc := range cases {
		if err := validateVolumeArchiveMeta(tc.size, tc.sha); (err == nil) != tc.ok {
			t.Errorf("validateVolumeArchiveMeta(%d, %q) err = %v, want ok=%v", tc.size, tc.sha, err, tc.ok)
		}
	}
}

// ---- purge volume slot (P1-3) ----

func TestPurgeVolumeArchivesCallerHoldsSlot(t *testing.T) {
	e, store, _ := newVolumeTestEngine(t)
	app := &Application{ID: "app1", HostID: "h1", SourceType: "image"}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	// The purge flow acquires the volume slot at entry and holds it for the
	// whole purge, so PurgeVolumeArchives must run under the caller's slot:
	// it must succeed (not self-deadlock on a non-reentrant acquire) and
	// must leave the caller's slot untouched for the caller to release.
	// A backup/restore racing the purge is refused earlier — deleteApp
	// returns 409 when VolumeBusy, and purgeAppResources itself fails its
	// entry acquire — never inside this function.
	if !e.acquireVolumeSlot(app.ID) {
		t.Fatal("test setup: acquire slot")
	}
	if err := e.PurgeVolumeArchives(app); err != nil {
		t.Fatalf("purge under held slot: %v", err)
	}
	if !e.VolumeBusy(app.ID) {
		t.Fatal("purge must not release the caller's slot")
	}
	e.releaseVolumeSlot(app.ID)
	if e.VolumeBusy(app.ID) {
		t.Fatal("caller release must free the slot")
	}
}
