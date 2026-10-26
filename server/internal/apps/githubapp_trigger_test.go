package apps

import (
	"errors"
	"testing"

	"watchman/server/internal/githubapp"
)

type fakeAppLister struct{ apps []*Application }

func (f *fakeAppLister) ListApps() []*Application { return f.apps }

type fakeStarter struct {
	running map[string]bool
	started []*Application
	failErr error
}

func (f *fakeStarter) IsRunning(appID string) bool { return f.running[appID] }

func (f *fakeStarter) StartDeployment(app *Application, trigger, startedBy string) (*Deployment, error) {
	if f.failErr != nil {
		return nil, f.failErr
	}
	f.started = append(f.started, app)
	return &Deployment{ID: "dep-" + app.ID}, nil
}

func TestGitHubAppTriggerPush(t *testing.T) {
	apps := []*Application{
		{ID: "a1", Name: "web", RepoURL: "https://github.com/acme/web.git", Branch: "main", AutoDeploy: true},
		{ID: "a2", Name: "web-staging", RepoURL: "git@github.com:acme/web.git", Branch: "staging", AutoDeploy: true},
		{ID: "a3", Name: "web-manual", RepoURL: "https://github.com/acme/web", Branch: "main", AutoDeploy: false},
		{ID: "a4", Name: "other", RepoURL: "https://github.com/acme/other", Branch: "main", AutoDeploy: true},
		// Bare "owner/repo" stored for another host must never match a
		// GitHub push to the same-named repository.
		{ID: "a5", Name: "web-elsewhere", RepoURL: "acme/web", Branch: "main", AutoDeploy: true},
	}
	starter := &fakeStarter{running: map[string]bool{}}
	trigger := &GitHubAppTrigger{Apps: &fakeAppLister{apps: apps}, Starter: starter}

	results := trigger.TriggerPush("acme/web", "main")
	if len(results) != 3 {
		t.Fatalf("expected 3 repo-matched results, got %d: %+v", len(results), results)
	}
	byID := map[string]githubapp.TriggerResult{}
	for _, r := range results {
		byID[r.AppID] = r
	}
	if byID["a1"].DeploymentID != "dep-a1" {
		t.Errorf("a1 should have deployed, got %+v", byID["a1"])
	}
	if byID["a2"].Skipped == "" {
		t.Errorf("a2 (staging branch) should skip a main push, got %+v", byID["a2"])
	}
	if byID["a3"].Skipped == "" {
		t.Errorf("a3 (auto_deploy off) should be skipped, got %+v", byID["a3"])
	}
	if len(starter.started) != 1 || starter.started[0].ID != "a1" {
		t.Fatalf("only a1 should start, got %v", starter.started)
	}

	// Branch mismatch: main push must not trigger the staging app.
	results = trigger.TriggerPush("acme/web", "staging")
	if len(results) != 3 {
		t.Fatalf("expected 3 results on staging push, got %d", len(results))
	}
	for _, r := range results {
		if r.AppID == "a2" && r.DeploymentID == "" {
			t.Errorf("a2 should deploy on staging push, got %+v", r)
		}
		if r.AppID == "a1" && r.Skipped == "" {
			t.Errorf("a1 should skip a staging push, got %+v", r)
		}
	}

	// In-progress deployment blocks a second trigger.
	starter.running["a1"] = true
	results = trigger.TriggerPush("acme/web", "main")
	for _, r := range results {
		if r.AppID == "a1" && r.Skipped == "" {
			t.Errorf("a1 should skip while a deployment is running, got %+v", r)
		}
	}

	// Unknown repo triggers nothing.
	if results := trigger.TriggerPush("acme/unknown", "main"); len(results) != 0 {
		t.Fatalf("unknown repo should trigger nothing, got %+v", results)
	}

	// Starter failure surfaces as a skip, not a panic.
	starter.running = map[string]bool{}
	starter.failErr = errors.New("engine busy")
	results = trigger.TriggerPush("acme/web", "main")
	for _, r := range results {
		if r.AppID == "a1" && r.Skipped != "engine busy" {
			t.Errorf("a1 failure should surface as skip, got %+v", r)
		}
	}
}
