package backup

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"watchman/server/internal/snapshots"
)

func awaitTask(t *testing.T, s *Store) Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status := s.Status()
		if status.Status != "running" {
			s.opMu.Lock()
			s.opMu.Unlock()
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not finish")
	return Task{}
}

func TestTasksBusyPendingAndInterrupted(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)
	s.opMu.Lock()
	if _, err := s.StartBackup("admin", "", "local", false); err != ErrBusy {
		t.Errorf("busy backup=%v", err)
	}
	if _, err := s.Delete("missing"); err != ErrBusy {
		t.Errorf("busy delete=%v", err)
	}
	s.opMu.Unlock()
	if _, err := s.StartBackup("admin", "", "local", false); err != nil {
		t.Fatal(err)
	}
	status := awaitTask(t, s)
	if status.Status != "success" || status.Archive == nil {
		t.Fatalf("backup task=%+v", status)
	}
	if _, err := s.StartRestoreFile(status.Archive.Name, "admin", ""); err != nil {
		t.Fatal(err)
	}
	status = awaitTask(t, s)
	if status.Status != "ready" || !status.Result.Pending {
		t.Fatalf("restore task=%+v", status)
	}
	if _, err := s.StartBackup("admin", "", "local", false); err != ErrPending {
		t.Errorf("pending backup=%v", err)
	}
	if err := s.CancelPending(); err != nil {
		t.Fatal(err)
	}
	if s.Status().Status != "cancelled" {
		t.Fatal("cancel status missing")
	}
	s.task = Task{ID: "interrupted", Status: "running", Kind: "backup"}
	if err := s.saveTaskLocked(); err != nil {
		t.Fatal(err)
	}
	boot, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if boot.Status().Status != "failed" {
		t.Fatal("interrupted task still running")
	}
}

func TestS3ReplicationRetentionAndFailure(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	reject := false
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if reject {
			w.WriteHeader(503)
			return
		}
		switch r.Method {
		case http.MethodPut:
			raw, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = raw
			w.WriteHeader(200)
		case http.MethodDelete:
			delete(objects, r.URL.Path)
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}
	}))
	defer remote.Close()
	s, dir := newTestStore(t)
	writeSeed(t, dir)
	targets, err := snapshots.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := targets.PutS3Target(&snapshots.S3Target{ID: "test", Name: "fixture", Endpoint: remote.URL, Bucket: "bucket", AccessKey: "fixture", SecretKey: "fixture", ForcePathStyle: true}); err != nil {
		t.Fatal(err)
	}
	s.SetS3Targets(targets)
	if err := s.SavePolicy(Policy{Time: "03:00", Retention: 1, TargetID: "test"}); err != nil {
		t.Fatal(err)
	}
	manual, err := s.Create("admin", "manual", false)
	if err != nil {
		t.Fatal(err)
	}
	safety, err := s.Create("admin", "safety", true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.StartBackup("system", "scheduled", "test", true); err != nil {
			t.Fatal(err)
		}
		if st := awaitTask(t, s); st.Status != "success" {
			t.Fatalf("scheduled task=%+v", st)
		}
	}
	if len(s.List()) != 3 {
		t.Fatalf("retention deleted manual/safety or failed to prune: %+v", s.List())
	}
	if _, err := s.Path(manual.Name); err != nil {
		t.Fatal("manual backup pruned")
	}
	if _, err := s.Path(safety.Name); err != nil {
		t.Fatal("safety backup pruned")
	}
	mu.Lock()
	if len(objects) != 1 {
		t.Errorf("S3 retention left %d copies", len(objects))
	}
	reject = true
	mu.Unlock()
	if _, err := s.StartBackup("admin", "failure", "test", false); err != nil {
		t.Fatal(err)
	}
	status := awaitTask(t, s)
	if status.Status != "failed" || status.Archive == nil || status.Archive.RemoteError == "" {
		t.Fatalf("upload failure not reported: %+v", status)
	}
	if _, err := s.Path(status.Archive.Name); err != nil {
		t.Fatal("failed S3 copy removed local safety copy")
	}
	if _, err := s.Delete(status.Archive.Name); err == nil {
		t.Fatal("failed remote deletion silently discarded metadata")
	}
	mu.Lock()
	reject = false
	mu.Unlock()
	warning, err := s.Delete(status.Archive.Name)
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Fatalf("unexpected delete warning: %s", warning)
	}
	// A removed S3 target is permanent: local deletion must fall through with
	// a warning instead of blocking the archive forever.
	if _, err := s.StartBackup("admin", "remote-copy", "test", false); err != nil {
		t.Fatal(err)
	}
	status = awaitTask(t, s)
	if status.Status != "success" || status.Archive == nil || status.Archive.RemoteKey == "" {
		t.Fatalf("remote backup task=%+v", status)
	}
	if err := targets.DeleteS3Target("test"); err != nil {
		t.Fatal(err)
	}
	warning, err = s.Delete(status.Archive.Name)
	if err != nil {
		t.Fatalf("removed target must not block deletion: %v", err)
	}
	if warning == "" {
		t.Fatal("removed target must yield a delete warning")
	}
	if _, err := os.Stat(filepath.Join(s.backupDir, status.Archive.Name)); !os.IsNotExist(err) {
		t.Fatal("archive survived deletion after target removal")
	}
}

func TestScheduleDispatchesOnceAndPolicyPersists(t *testing.T) {
	s, dir := newTestStore(t)
	writeSeed(t, dir)
	now := time.Now()
	if err := s.SavePolicy(Policy{Enabled: true, Time: now.Format("15:04"), Retention: 7, TargetID: "local"}); err != nil {
		t.Fatal(err)
	}
	s.runDue(now)
	if st := awaitTask(t, s); st.Status != "success" {
		t.Fatalf("schedule=%+v", st)
	}
	s.runDue(now.Add(time.Second))
	if len(s.List()) != 1 {
		t.Fatal("schedule fired twice in one minute")
	}
	boot, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !boot.Policy().Enabled || boot.Policy().LastRun.IsZero() {
		t.Fatal("policy/run state not persisted")
	}
	for _, p := range []Policy{{Time: "25:00", Retention: 7}, {Time: "03:00", Retention: 0}, {Time: "03:00", Retention: 7, TargetID: "missing"}} {
		if err := s.SavePolicy(p); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}

func TestUploadTaskAndSecretsNeverInStatus(t *testing.T) {
	s, dir := newTestStore(t)
	EnsureServerSecrets(dir, "private-jwt-fixture", "private-vault-fixture")
	archive := buildArchive(t, map[string]string{"users.json": "[]"})
	if _, err := s.StartRestoreUpload(bytes.NewReader(archive), "admin", ""); err != nil {
		t.Fatal(err)
	}
	status := awaitTask(t, s)
	if status.Status != "ready" {
		t.Fatalf("upload status=%+v", status)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "private-") {
		t.Fatal("status exposed root secrets")
	}
	if err := s.CancelPending(); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(s.backupDir)
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".upload-") || strings.HasPrefix(f.Name(), ".restore-") {
			t.Fatalf("staging leaked: %s", f.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, secretsFile)); err != nil {
		t.Fatal("cancel removed live keys")
	}
}
