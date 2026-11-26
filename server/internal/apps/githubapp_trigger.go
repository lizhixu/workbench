package apps

import (
	"fmt"
	"log/slog"
	"strings"

	"watchman/server/internal/githubapp"
)

// AppLister abstracts the app store for push routing tests.
type AppLister interface {
	ListApps() []*Application
}

// TriggerRecorder persists push / CI evaluation events for diagnostics.
type TriggerRecorder interface {
	AddTriggerEvent(ev *TriggerEvent) error
}

// DeploymentStarter abstracts the deployment engine for push routing tests.
type DeploymentStarter interface {
	IsRunning(appID string) bool
	StartDeployment(app *Application, trigger, startedBy string) (*Deployment, error)
}

// GitHubAppTrigger routes a GitHub App push delivery to matching Git apps
// and records both triggered and skipped outcomes per app.
type GitHubAppTrigger struct {
	Apps     AppLister
	Triggers TriggerRecorder
	Starter  DeploymentStarter
}

func (t *GitHubAppTrigger) TriggerPush(ev *githubapp.PushEvent) []githubapp.TriggerResult {
	branch := ev.Branch()
	var results []githubapp.TriggerResult
	for _, app := range t.Apps.ListApps() {
		if app.SourceType != "" && app.SourceType != "git" {
			continue
		}
		// Only github.com remotes can match a GitHub push: a bare
		// "owner/repo" string stored for another host must never deploy
		// on a same-named GitHub repository's push.
		if !strings.Contains(strings.ToLower(app.RepoURL), "github.com") {
			continue
		}
		if githubapp.NormalizeRepoURL(app.RepoURL) != strings.ToLower(ev.RepoFullName) {
			continue
		}
		res := githubapp.TriggerResult{AppID: app.ID, AppName: app.Name}
		record := &TriggerEvent{
			AppID:      app.ID,
			Source:     "github-app",
			DeliveryID: ev.DeliveryID,
			Ref:        branch,
			Commit:     ev.CommitSHA,
			Actor:      ev.Actor,
		}
		targetBranch := app.Branch
		if targetBranch == "" {
			targetBranch = "main"
		}
		switch {
		case !app.AutoDeploy:
			res.Skipped = "auto deploy disabled for this app"
			record.Outcome = TriggerSkipped
			record.Reason = "自动部署未开启"
		case !sameBranch(branch, app.Branch):
			res.Skipped = "branch does not match " + app.Branch
			record.Outcome = TriggerSkipped
			record.Reason = fmt.Sprintf("监听分支为 %s，本次推送为 %s", targetBranch, branch)
		case len(app.WatchPaths) > 0 && ev.ChangedPathsKnown && !WatchPathsMatch(app.WatchPaths, ev.ChangedPaths):
			res.Skipped = "no watched paths changed"
			record.Outcome = TriggerSkipped
			record.Reason = fmt.Sprintf("未命中监听目录（变更：%s）", samplePaths(ev.ChangedPaths, 3))
		case t.Starter.IsRunning(app.ID):
			res.Skipped = "another deployment is already running"
			record.Outcome = TriggerSkipped
			record.Reason = "已有部署任务正在运行，跳过本次触发"
		default:
			if len(app.WatchPaths) > 0 && !ev.ChangedPathsKnown {
				slog.Info("watch_paths 已配置但本次推送未携带变更文件清单，照常部署", "app", app.ID, "repo", ev.RepoFullName)
			}
			actor := ev.Actor
			if actor == "" {
				actor = "github-app"
			}
			dep, err := t.Starter.StartDeployment(app, "github-app", actor)
			if err != nil {
				res.Skipped = err.Error()
				record.Outcome = TriggerSkipped
				record.Reason = "触发失败: " + err.Error()
			} else {
				res.DeploymentID = dep.ID
				record.Outcome = TriggerTriggered
				record.DeploymentID = dep.ID
				record.Reason = fmt.Sprintf("推送分支 %s 触发部署", branch)
			}
		}
		if t.Triggers != nil {
			_ = t.Triggers.AddTriggerEvent(record)
		}
		results = append(results, res)
	}
	if results == nil {
		results = []githubapp.TriggerResult{}
	}
	return results
}

func samplePaths(paths []string, limit int) string {
	if len(paths) == 0 {
		return "无文件改动"
	}
	if len(paths) <= limit {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s 等 %d 项", strings.Join(paths[:limit], ", "), len(paths))
}
