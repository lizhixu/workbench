package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
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
		name      string
		preflight error
		want      int
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

func TestUpdateAppVolumeJobRefreshesHostID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	host := &fakeAppVolumeHost{hostID: "host_new", volumes: []string{"/opt/a:/a"}}
	r, store := newAppVolumeHandlerTest(t, host)
	job := &Job{ID: "bk_1", Name: "j", HostID: "host_old", Kind: KindAppVolume, AppID: "app1",
		Target: "/opt/a:/a", StorageTarget: "default", Retention: 7, Enabled: true}
	if err := store.PutJob(job); err != nil {
		t.Fatal(err)
	}
	body := `{"enabled":false}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/backups/jobs/bk_1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update app_volume job: code=%d body=%s", w.Code, w.Body.String())
	}
	got, ok := store.GetJob("bk_1")
	if !ok || got.HostID != "host_new" {
		t.Fatalf("app_volume job HostID = %q, want host_new", got.HostID)
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

// newAppVolumeHandlerTestWithAgent builds the handler stack on a real registry
// with one online agent, so createJob's agent-online gate passes and the
// stored job can be asserted.
func newAppVolumeHandlerTestWithAgent(t *testing.T, host *fakeAppVolumeHost) (*gin.Engine, *Store) {
	t.Helper()
	_, store := newAppVolumeHandlerTest(t, host)
	reg := rpc.NewRegistry(t.TempDir(), nil)
	tok := reg.IssueEnrollToken()
	if _, _, err := reg.Register(context.Background(), &agentpb.RegisterRequest{
		EnrollToken: tok, AgentId: host.hostID, Hostname: host.hostID, Os: "linux", Arch: "amd64",
	}, ""); err != nil {
		t.Fatalf("Register %s: %v", host.hostID, err)
	}
	engine := NewEngine(nil, store, nil)
	engine.SetAppVolumeHost(host)
	h := NewHandlers(reg, store, engine)
	r := gin.New()
	h.Register(r.Group(""), r.Group(""))
	return r, store
}

func postCreateAppVolumeJob(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/backups/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// TestCreateAppVolumeJobMultiSelect locks the job target contract end to end:
// "ALL" (whole-app job) and comma-joined subsets store canonically, an
// undeclared spec or a blank list is rejected up front.
func TestCreateAppVolumeJobMultiSelect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	host := &fakeAppVolumeHost{hostID: "h1", volumes: []string{"/opt/a:/a", "/opt/b:/b"}}
	r, store := newAppVolumeHandlerTestWithAgent(t, host)

	w := postCreateAppVolumeJob(t, r, `{"name":"whole","host_id":"","kind":"app_volume","app_id":"app1","target":"ALL","storage_target":"s3_t","cron":"0 3 * * *","enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create ALL job: code=%d body=%s", w.Code, w.Body.String())
	}

	// Find the stored job by name (IDs are random).
	var whole, subset *Job
	for _, j := range store.ListJobs() {
		switch j.Name {
		case "whole":
			whole = j
		case "subset":
			subset = j
		}
	}
	if whole == nil {
		t.Fatal("ALL job not stored")
	}
	if whole.Target != "ALL" {
		t.Fatalf("ALL job target = %q, want ALL", whole.Target)
	}

	// Comma-joined subset with blanks/duplicates normalizes to the canonical list.
	w = postCreateAppVolumeJob(t, r, `{"name":"subset","host_id":"","kind":"app_volume","app_id":"app1","target":" /opt/a:/a , /opt/b:/b ,, /opt/a:/a ","storage_target":"s3_t","cron":"","enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create subset job: code=%d body=%s", w.Code, w.Body.String())
	}
	for _, j := range store.ListJobs() {
		if j.Name == "subset" {
			subset = j
		}
	}
	if subset == nil {
		t.Fatal("subset job not stored")
	}
	if subset.Target != "/opt/a:/a,/opt/b:/b" {
		t.Fatalf("subset job target = %q, want normalized join", subset.Target)
	}

	// Undeclared spec inside a multi selection is rejected.
	w = postCreateAppVolumeJob(t, r, `{"name":"bad","host_id":"","kind":"app_volume","app_id":"app1","target":"/opt/a:/a,/opt/x:/x","storage_target":"s3_t","enabled":true}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "只能选择应用已声明的挂载卷") {
		t.Fatalf("undeclared spec: code=%d body=%s", w.Code, w.Body.String())
	}

	// A list that parses to nothing is rejected as "no volume selected".
	w = postCreateAppVolumeJob(t, r, `{"name":"bad2","host_id":"","kind":"app_volume","app_id":"app1","target":" , , ","storage_target":"s3_t","enabled":true}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "必须选择挂载卷") {
		t.Fatalf("blank list: code=%d body=%s", w.Code, w.Body.String())
	}
}

// TestUpdateAppVolumeJobTargetNormalizes exercises the partial-update path:
// a target change re-validates the allowlist and stores the canonical form.
func TestUpdateAppVolumeJobTargetNormalizes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	host := &fakeAppVolumeHost{hostID: "h1", volumes: []string{"/opt/a:/a", "/opt/b:/b"}}
	r, store := newAppVolumeHandlerTestWithAgent(t, host)
	job := &Job{ID: "bk_1", Name: "j", HostID: "h1", Kind: KindAppVolume, AppID: "app1",
		Target: "/opt/a:/a", StorageTarget: "s3_t", Retention: 7, Enabled: true, Schedule: Schedule{Cron: "0 3 * * *"}}
	if err := store.PutJob(job); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"target": " /opt/a:/a , /opt/b:/b "})
	req := httptest.NewRequest(http.MethodPut, "/backups/jobs/bk_1", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update target: code=%d body=%s", w.Code, w.Body.String())
	}
	got, _ := store.GetJob("bk_1")
	if got.Target != "/opt/a:/a,/opt/b:/b" {
		t.Fatalf("updated target = %q, want normalized join", got.Target)
	}

	// Switching the job to whole-app via any case form stores the ALL sentinel.
	body, _ = json.Marshal(map[string]any{"target": "all"})
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/backups/jobs/bk_1", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update to ALL: code=%d body=%s", w.Code, w.Body.String())
	}
	got, _ = store.GetJob("bk_1")
	if got.Target != AppVolumeTargetAll {
		t.Fatalf("updated target = %q, want ALL", got.Target)
	}

	// Undeclared target on update is rejected and keeps the previous value.
	body, _ = json.Marshal(map[string]any{"target": "/opt/x:/x"})
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/backups/jobs/bk_1", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("undeclared update: code=%d body=%s", w.Code, w.Body.String())
	}
	got, _ = store.GetJob("bk_1")
	if got.Target != AppVolumeTargetAll {
		t.Fatalf("rejected update must keep ALL, got %q", got.Target)
	}
}
