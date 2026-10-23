package snapshots

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeAppVolumeHost implements AppVolumeHost for handler tests.
type fakeAppVolumeHost struct {
	volumes   []string
	hostID    string
	preflight error

	mu       sync.Mutex
	restored []string
}

func (f *fakeAppVolumeHost) takeRestored() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.restored))
	copy(out, f.restored)
	return out
}

func (f *fakeAppVolumeHost) ResolveAppVolume(appID string) (*AppVolumeTarget, error) {
	return &AppVolumeTarget{AppID: appID, Name: "app", HostID: f.hostID, Volumes: f.volumes}, nil
}

func (f *fakeAppVolumeHost) CheckAppVolumeRestore(appID string) error { return f.preflight }

func (f *fakeAppVolumeHost) RunAppVolumeBackup(appID, spec, note, storageTarget string, retention int) error {
	return nil
}

func (f *fakeAppVolumeHost) ListAppVolumeArchives(appID, spec string) []*AppVolumeArchive {
	return []*AppVolumeArchive{{
		ID: "vbk_ok", HostID: f.hostID, VolumeSpec: spec,
		StorageTarget: "s3_t", StorageTargetName: "t", StoragePath: "apps/app1/vbk_ok.tar.gz",
		Status: appVolumeStatusSuccess,
	}}
}

func (f *fakeAppVolumeHost) RestoreAppVolumeByID(appID, archiveID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored = append(f.restored, archiveID)
	return nil
}

func (f *fakeAppVolumeHost) DeleteAppVolumeByID(appID, archiveID string) error { return nil }

func newAppVolumeHandlerTest(t *testing.T, host *fakeAppVolumeHost) (*gin.Engine, *Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.PutS3Target(&S3Target{ID: "s3_t", Name: "t", Bucket: "b", IsDefault: true}); err != nil {
		t.Fatalf("PutS3Target: %v", err)
	}
	engine := NewEngine(nil, store, nil)
	engine.SetAppVolumeHost(host)
	h := NewHandlers(nil, store, engine)
	r := gin.New()
	h.Register(r.Group(""), r.Group(""))
	return r, store
}

func TestRestoreArchivePreflightRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name     string
		preflight error
		want     int
	}{
		{"busy", ErrVolumeRestoreBusy, http.StatusConflict},
		{"deploying", ErrVolumeRestoreDeploying, http.StatusConflict},
		{"offline", ErrVolumeRestoreOffline, http.StatusServiceUnavailable},
		{"other", errors.New("Compose 应用不支持"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeAppVolumeHost{hostID: "h1", volumes: []string{"/opt/a:/a"}, preflight: tc.preflight}
			r, store := newAppVolumeHandlerTest(t, host)
			job := &Job{ID: "bk_1", Name: "j", HostID: "h1", Kind: KindAppVolume, AppID: "app1",
				Target: "/opt/a:/a", Retention: 7, Enabled: true}
			if err := store.PutJob(job); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
				"/backups/jobs/bk_1/archives/vbk_ok/restore", nil))
			if w.Code != tc.want {
				t.Fatalf("restore preflight: code=%d body=%s, want %d", w.Code, w.Body.String(), tc.want)
			}
			if got := host.takeRestored(); len(got) != 0 {
				t.Fatalf("rejected restore must not run: %v", got)
			}
		})
	}
}

func TestRestoreArchiveAcceptedAfterPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	host := &fakeAppVolumeHost{hostID: "h1", volumes: []string{"/opt/a:/a"}}
	r, store := newAppVolumeHandlerTest(t, host)
	job := &Job{ID: "bk_1", Name: "j", HostID: "h1", Kind: KindAppVolume, AppID: "app1",
		Target: "/opt/a:/a", Retention: 7, Enabled: true}
	if err := store.PutJob(job); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		"/backups/jobs/bk_1/archives/vbk_ok/restore", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("accepted restore: code=%d body=%s", w.Code, w.Body.String())
	}
	// The restore runs in a goroutine; poll briefly for it to be dispatched.
	var dispatched []string
	for i := 0; i < 100; i++ {
		dispatched = host.takeRestored()
		if len(dispatched) != 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(dispatched) != 1 || dispatched[0] != "vbk_ok" {
		t.Fatalf("restore not dispatched: %v", dispatched)
	}
}

func TestUpdateJobPartialTargetKeepsExisting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	host := &fakeAppVolumeHost{hostID: "h1", volumes: []string{"/opt/a:/a"}}
	r, store := newAppVolumeHandlerTest(t, host)
	job := &Job{ID: "bk_1", Name: "j", HostID: "h1", Kind: KindAppVolume, AppID: "app1",
		Target: "/opt/a:/a", Retention: 7, Enabled: true, Schedule: Schedule{Cron: "0 3 * * *"}}
	if err := store.PutJob(job); err != nil {
		t.Fatal(err)
	}
	// Toggle-only update: no target/storage_target/cron in the body.
	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"enabled": false})
	req := httptest.NewRequest(http.MethodPut, "/backups/jobs/bk_1", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial update: code=%d body=%s", w.Code, w.Body.String())
	}
	got, ok := store.GetJob("bk_1")
	if !ok {
		t.Fatal("job vanished")
	}
	if got.Target != "/opt/a:/a" {
		t.Fatalf("partial update must keep the target, got %q", got.Target)
	}
	if got.Schedule.Cron != "0 3 * * *" {
		t.Fatalf("partial update must keep the cron, got %q", got.Schedule.Cron)
	}
	if got.Enabled {
		t.Fatal("explicit enabled=false must be applied")
	}

	// Name-only update keeps enabled and writes the new name.
	w = httptest.NewRecorder()
	body, _ = json.Marshal(map[string]any{"name": "renamed"})
	req = httptest.NewRequest(http.MethodPut, "/backups/jobs/bk_1", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("name update: code=%d body=%s", w.Code, w.Body.String())
	}
	got, _ = store.GetJob("bk_1")
	if got.Name != "renamed" || got.Enabled {
		t.Fatalf("name update: name=%q enabled=%v", got.Name, got.Enabled)
	}

	// Explicitly blanking the target/name is rejected, not stored.
	for _, bad := range []map[string]any{
		{"target": ""},
		{"target": "   "},
		{"name": ""},
		{"retention": 101},
	} {
		w = httptest.NewRecorder()
		body, _ = json.Marshal(bad)
		req = httptest.NewRequest(http.MethodPut, "/backups/jobs/bk_1", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid update %v: code=%d body=%s", bad, w.Code, w.Body.String())
		}
	}
	got, _ = store.GetJob("bk_1")
	if got.Target != "/opt/a:/a" || got.Name != "renamed" {
		t.Fatalf("rejected updates must not mutate the job: %+v", got)
	}
}
