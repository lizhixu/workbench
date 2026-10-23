package snapshots

import "testing"

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

	if got := s.DeleteJobsByApp("app_1"); got != 2 {
		t.Fatalf("DeleteJobsByApp = %d, want 2", got)
	}
	if got := s.DeleteJobsByApp("app_missing"); got != 0 {
		t.Fatalf("DeleteJobsByApp on unknown app = %d, want 0", got)
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
