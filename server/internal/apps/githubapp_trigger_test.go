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

	results := trigger.TriggerPush(pushEvent("acme/web", "main"))
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
	results = trigger.TriggerPush(pushEvent("acme/web", "staging"))
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
	results = trigger.TriggerPush(pushEvent("acme/web", "main"))
	for _, r := range results {
		if r.AppID == "a1" && r.Skipped == "" {
			t.Errorf("a1 should skip while a deployment is running, got %+v", r)
		}
	}

	// Unknown repo triggers nothing.
	if results := trigger.TriggerPush(pushEvent("acme/unknown", "main")); len(results) != 0 {
		t.Fatalf("unknown repo should trigger nothing, got %+v", results)
	}

	// Starter failure surfaces as a skip, not a panic.
	starter.running = map[string]bool{}
	starter.failErr = errors.New("engine busy")
	results = trigger.TriggerPush(pushEvent("acme/web", "main"))
	for _, r := range results {
		if r.AppID == "a1" && r.Skipped != "engine busy" {
			t.Errorf("a1 failure should surface as skip, got %+v", r)
		}
	}
}

// pushEvent builds a push delivery for tests: a branch push with a known
// (possibly empty) changed-file list.
func pushEvent(repo, branch string) *githubapp.PushEvent {
	return &githubapp.PushEvent{
		RepoFullName:      repo,
		Ref:               "refs/heads/" + branch,
		ChangedPathsKnown: true,
	}
}

func TestGitHubAppTriggerWatchPaths(t *testing.T) {
	apps := []*Application{
		{ID: "w1", Name: "web", RepoURL: "https://github.com/acme/web.git", Branch: "main", AutoDeploy: true, WatchPaths: []string{"apps/web"}},
		{ID: "w2", Name: "api", RepoURL: "https://github.com/acme/web.git", Branch: "main", AutoDeploy: true, WatchPaths: []string{"apps/api", "docker-compose.yml"}},
	}
	starter := &fakeStarter{running: map[string]bool{}}
	trigger := &GitHubAppTrigger{Apps: &fakeAppLister{apps: apps}, Starter: starter}
	byID := func(results []githubapp.TriggerResult) map[string]githubapp.TriggerResult {
		m := map[string]githubapp.TriggerResult{}
		for _, r := range results {
			m[r.AppID] = r
		}
		return m
	}

	// Push touching only apps/web: w1 deploys, w2 skips.
	ev := pushEvent("acme/web", "main")
	ev.ChangedPaths = []string{"apps/web/src/index.ts", "README.md"}
	res := byID(trigger.TriggerPush(ev))
	if res["w1"].DeploymentID == "" {
		t.Errorf("w1 should deploy on watched-path push, got %+v", res["w1"])
	}
	if res["w2"].Skipped != "no watched paths changed" {
		t.Errorf("w2 should skip an untouched push, got %+v", res["w2"])
	}
	if len(starter.started) != 1 || starter.started[0].ID != "w1" {
		t.Fatalf("only w1 should start, got %v", starter.started)
	}

	// Push touching the shared compose file: w2 deploys, w1 skips.
	starter.started = nil
	ev.ChangedPaths = []string{"docker-compose.yml"}
	res = byID(trigger.TriggerPush(ev))
	if res["w2"].DeploymentID == "" {
		t.Errorf("w2 should deploy on compose-file push, got %+v", res["w2"])
	}
	if res["w1"].Skipped != "no watched paths changed" {
		t.Errorf("w1 should skip a compose-only push, got %+v", res["w1"])
	}

	// Unknown file list (truncated / provider without lists): fail-open,
	// both deploy as before.
	starter.started = nil
	ev.ChangedPaths = nil
	ev.ChangedPathsKnown = false
	res = byID(trigger.TriggerPush(ev))
	if res["w1"].DeploymentID == "" || res["w2"].DeploymentID == "" {
		t.Errorf("unknown file list should deploy both, got %+v", res)
	}
}
