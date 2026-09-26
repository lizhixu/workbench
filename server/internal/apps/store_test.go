package apps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestStoreAppRoundTrip(t *testing.T) {
	s := newTestStore(t)
	app := &Application{
		ID:      "app_test",
		Name:    "demo",
		HostID:  "host1",
		RepoURL: "https://github.com/example/repo.git",
		Branch:  "main",
		EnvVars: map[string]string{"KEY": "value"},
		Ports:   []PortMapping{{Host: 8080, Container: 80}},
	}
	if err := s.PutApp(app); err != nil {
		t.Fatalf("PutApp: %v", err)
	}
	if err := s.DeleteApp("app_missing"); err == nil {
		t.Fatal("deleting a missing app should fail")
	}

	// Reload from disk to prove the atomic-file persistence works.
	s2, err := NewStore(s.dir, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := s2.GetApp("app_test")
	if !ok {
		t.Fatal("app not found after reload")
	}
	if got.Name != "demo" || got.RepoURL != app.RepoURL || len(got.Ports) != 1 {
		t.Fatalf("app fields mismatch: %+v", got)
	}

	// Update path.
	if err := s2.UpdateApp("app_test", func(a *Application) error {
		a.Branch = "release"
		return nil
	}); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	if a, _ := s2.GetApp("app_test"); a.Branch != "release" {
		t.Fatalf("UpdateApp did not persist: %s", a.Branch)
	}
}

func TestStoreWebhookLookup(t *testing.T) {
	s := newTestStore(t)
	_ = s.PutApp(&Application{ID: "a1", Name: "x", WebhookToken: "tok1", CreatedAt: time.Now()})
	if _, ok := s.FindAppByWebhookToken("wrong"); ok {
		t.Fatal("unknown token must not resolve")
	}
	if a, ok := s.FindAppByWebhookToken("tok1"); !ok || a.ID != "a1" {
		t.Fatal("token lookup failed")
	}
}

func TestStoreDeploymentsPagination(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 25; i++ {
		d := &Deployment{
			ID:        "dep" + string(rune('a'+i)),
			AppID:     "app_test",
			Status:    DeploySuccess,
			StartedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}
		if err := s.AddDeployment(d); err != nil {
			t.Fatalf("AddDeployment: %v", err)
		}
	}
	list, total := s.ListDeployments("app_test", 0, 20)
	if total != 25 || len(list) != 20 {
		t.Fatalf("page 1: total=%d len=%d", total, len(list))
	}
	list, total = s.ListDeployments("app_test", 20, 20)
	if total != 25 || len(list) != 5 {
		t.Fatalf("page 2: total=%d len=%d", total, len(list))
	}
	// Newest first.
	if list[0].StartedAt.Before(list[4].StartedAt) {
		t.Fatal("deployments not sorted newest-first")
	}
	// Mutation persists.
	if err := s.UpdateDeployment("app_test", "depa", func(d *Deployment) { d.Status = DeployFailed }); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if d, _ := s.GetDeployment("app_test", "depa"); d.Status != DeployFailed {
		t.Fatal("UpdateDeployment did not persist")
	}
}

func TestStoreDeploymentJSONLResilience(t *testing.T) {
	dir := t.TempDir()
	// One valid record, one truncated/corrupt tail line (crash mid-append).
	valid, _ := json.Marshal(&Deployment{ID: "d1", AppID: "a", Status: DeploySuccess, StartedAt: time.Now()})
	if err := os.WriteFile(filepath.Join(dir, deploymentsFile), append(append(valid, '\n'), []byte(`{"id":"d2","app_id":"a"`)...), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore with corrupt tail: %v", err)
	}
	list, total := s.ListDeployments("a", 0, 10)
	if total != 1 || len(list) != 1 || list[0].ID != "d1" {
		t.Fatalf("corrupt tail should drop only the broken record: total=%d", total)
	}
}
