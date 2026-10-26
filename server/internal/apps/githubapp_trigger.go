package apps

import (
	"strings"

	"watchman/server/internal/githubapp"
)

// AppLister abstracts the app store for push routing tests.
type AppLister interface {
	ListApps() []*Application
}

// DeploymentStarter abstracts the deployment engine for push routing tests.
type DeploymentStarter interface {
	IsRunning(appID string) bool
	StartDeployment(app *Application, trigger, startedBy string) (*Deployment, error)
}

// GitHubAppTrigger routes a GitHub App push delivery to deployments. It
// mirrors the per-app webhook's gates (auto_deploy switch, branch match,
// in-progress 409) but matches on the repository name instead of a
// per-app token, so one delivery can fan out to several apps on the same
// repository (e.g. different branches = different environments).
type GitHubAppTrigger struct {
	Apps    AppLister
	Starter DeploymentStarter
}

func (t *GitHubAppTrigger) TriggerPush(repoFullName, branch string) []githubapp.TriggerResult {
	var results []githubapp.TriggerResult
	for _, app := range t.Apps.ListApps() {
		// Only github.com remotes can match a GitHub push: a bare
		// "owner/repo" string stored for another host must never deploy
		// on a same-named GitHub repository's push.
		if !strings.Contains(strings.ToLower(app.RepoURL), "github.com") {
			continue
		}
		if githubapp.NormalizeRepoURL(app.RepoURL) != strings.ToLower(repoFullName) {
			continue
		}
		res := githubapp.TriggerResult{AppID: app.ID, AppName: app.Name}
		switch {
		case !app.AutoDeploy:
			res.Skipped = "auto deploy disabled for this app"
		case !sameBranch(branch, app.Branch):
			res.Skipped = "branch does not match " + app.Branch
		case t.Starter.IsRunning(app.ID):
			res.Skipped = "another deployment is already running"
		default:
			dep, err := t.Starter.StartDeployment(app, "github-app", "github-app")
			if err != nil {
				res.Skipped = err.Error()
			} else {
				res.DeploymentID = dep.ID
			}
		}
		results = append(results, res)
	}
	if results == nil {
		results = []githubapp.TriggerResult{}
	}
	return results
}
