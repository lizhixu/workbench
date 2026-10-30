package snapshots

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListJobsReturnsCopies(t *testing.T) {
	s, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.PutJob(&Job{ID: "job_1", Name: "original", Kind: KindDir, CreatedAt: timeNowForTest()}); err != nil {
		t.Fatal(err)
	}
	jobs := s.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("ListJobs = %d, want 1", len(jobs))
	}
	jobs[0].Name = "mutated"
	got, ok := s.GetJob("job_1")
	if !ok || got.Name != "original" {
		t.Fatalf("ListJobs leaked live job pointer: %+v", got)
	}
}

func TestDeleteJobsByAppPersistenceErrorRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	job := &Job{ID: "job_1", Kind: KindAppVolume, AppID: "app_1", HostID: "host_1"}
	if err := s.PutJob(job); err != nil {
		t.Fatal(err)
	}
	jobsPath := filepath.Join(dir, jobsFile)
	if err := os.Remove(jobsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(jobsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(jobsPath)
	if n, err := s.DeleteJobsByApp("app_1"); err == nil || n != 0 {
		t.Fatalf("DeleteJobsByApp = (%d, %v), want (0, error)", n, err)
	}
	if _, ok := s.GetJob(job.ID); !ok {
		t.Fatal("failed deletion removed the in-memory job")
	}
	if _, err := os.Stat(jobsPath); err != nil {
		t.Fatalf("jobs file disappeared after failed deletion: %v", err)
	}
}

func TestUpdateAppVolumeJobHost(t *testing.T) {
	s, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, job := range []*Job{
		{ID: "app_job", Kind: KindAppVolume, AppID: "app_1", HostID: "old"},
		{ID: "other_job", Kind: KindDir, AppID: "app_1", HostID: "old"},
	} {
		if err := s.PutJob(job); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateAppVolumeJobHost("app_1", "new"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob("app_job")
	if got.HostID != "new" {
		t.Fatalf("app_volume job host = %q, want new", got.HostID)
	}
	other, _ := s.GetJob("other_job")
	if other.HostID != "old" {
		t.Fatalf("non-app_volume job host changed to %q", other.HostID)
	}
}

// timeNowForTest keeps the fixture explicit without relying on zero-value ties.
func timeNowForTest() time.Time { return time.Now() }

func TestDeleteJobsByApp(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	seed := []*Job{
		{ID: "job_a1_data", Name: "a1 data", Kind: KindAppVolume, AppID: "app_1", Target: "/opt/a1/data:/data"},
		{ID: "job_a1_pg", Name: "a1 pg", Kind: KindAppVolume, AppID: "app_1", Target: "pg-vol:/var/lib/postgresql/data"},
		{ID: "job_a2", Name: "a2", Kind: KindAppVolume, AppID: "app_2", Target: "/opt/a2:/data"},
		// A host dir job that happens to carry an AppID must never be swept up:
		// only app_volume jobs are owned by the application.
		{ID: "job_dir", Name: "dir", Kind: KindDir, AppID: "app_1", HostID: "host_1", Target: "/etc"},
	}
	for _, j := range seed {
		if err := s.PutJob(j); err != nil {
			t.Fatalf("PutJob %s: %v", j.ID, err)
		}
	}

	if got, err := s.DeleteJobsByApp("app_1"); err != nil || got != 2 {
		t.Fatalf("DeleteJobsByApp = (%d, %v), want (2, nil)", got, err)
	}
	if got, err := s.DeleteJobsByApp("app_missing"); err != nil || got != 0 {
		t.Fatalf("DeleteJobsByApp on unknown app = (%d, %v), want (0, nil)", got, err)
	}
	for _, id := range []string{"job_a1_data", "job_a1_pg"} {
		if _, ok := s.GetJob(id); ok {
			t.Errorf("%s survived the cleanup", id)
		}
	}
	for _, id := range []string{"job_a2", "job_dir"} {
		if _, ok := s.GetJob(id); !ok {
			t.Errorf("%s was deleted but belongs to another owner", id)
		}
	}

	// The deletion must be on disk, not only in memory: a restart with the app
	// already gone would otherwise resurrect the cron entries.
	reopened, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("reopen NewStore: %v", err)
	}
	if _, ok := reopened.GetJob("job_a1_data"); ok {
		t.Error("deleted job persisted across restart")
	}
	if len(reopened.ListJobs()) != 2 {
		t.Errorf("ListJobs after restart = %d, want 2", len(reopened.ListJobs()))
	}
}
