package apps

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"watchman/server/internal/githubapp"
)

var validImageTagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// webhook handles Git provider push and CI image-update events.
// Authentication uses either the random per-app token in the path or
// Authorization: Bearer <token> (e.g. when calling /apps/webhook/_).
func (h *AppHandlers) webhook(c *gin.Context) {
	token := strings.TrimSpace(c.Param("token"))
	if token == "_" || token == "" {
		if bearer := bearerToken(c); bearer != "" {
			token = bearer
		}
	}
	h.handleWebhook(c, token)
}

// webhookGet supports `curl`-style trigger URLs.
func (h *AppHandlers) webhookGet(c *gin.Context) {
	token := strings.TrimSpace(c.Param("token"))
	if token == "_" || token == "" {
		if bearer := bearerToken(c); bearer != "" {
			token = bearer
		}
	}
	h.handleWebhook(c, token)
}

func bearerToken(c *gin.Context) string {
	hdr := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(hdr) > 7 && strings.EqualFold(hdr[:7], "Bearer ") {
		return strings.TrimSpace(hdr[7:])
	}
	return ""
}

func (h *AppHandlers) recordTrigger(ev *TriggerEvent) {
	if h.store != nil && ev != nil {
		_ = h.store.AddTriggerEvent(ev)
	}
}

func (h *AppHandlers) handleWebhook(c *gin.Context, token string) {
	app, ok := h.store.FindAppByWebhookToken(token)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown webhook token"})
		return
	}

	var body []byte
	if strings.HasPrefix(c.GetHeader("Content-Type"), "application/x-www-form-urlencoded") {
		body = []byte(c.PostForm("payload"))
	} else {
		var err error
		body, err = io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read body"})
			return
		}
	}

	// Image & Compose apps support CI / Actions webhooks (no Git branch matching).
	if app.SourceType == "image" || app.SourceType == "raw_compose" {
		h.handleCIWebhook(c, app, body)
		return
	}

	actor, commit := extractGitActorAndCommit(body)
	ev := &TriggerEvent{
		AppID:  app.ID,
		Source: "webhook",
		Commit: commit,
		Actor:  actor,
	}
	if !app.AutoDeploy {
		ev.Outcome = TriggerRejected
		ev.Reason = "自动部署未开启"
		h.recordTrigger(ev)
		c.JSON(http.StatusForbidden, gin.H{"error": "auto deploy disabled for this app"})
		return
	}

	branch := webhookBranch(c, body)
	ev.Ref = branch
	if branch == "" {
		ev.Outcome = TriggerRejected
		ev.Reason = "无法识别推送分支"
		h.recordTrigger(ev)
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot determine pushed branch"})
		return
	}
	targetBranch := app.Branch
	if targetBranch == "" {
		targetBranch = "main"
	}
	if !sameBranch(branch, app.Branch) {
		ev.Outcome = TriggerSkipped
		ev.Reason = fmt.Sprintf("监听分支为 %s，本次推送为 %s", targetBranch, branch)
		h.recordTrigger(ev)
		c.JSON(http.StatusOK, gin.H{"ok": true, "skipped": true,
			"reason": "branch does not match " + app.Branch})
		return
	}

	if len(app.WatchPaths) > 0 {
		changed, known := githubapp.ExtractChangedPaths(body)
		if known && !WatchPathsMatch(app.WatchPaths, changed) {
			ev.Outcome = TriggerSkipped
			ev.Reason = fmt.Sprintf("未命中监听目录（变更：%s）", samplePaths(changed, 3))
			h.recordTrigger(ev)
			c.JSON(http.StatusOK, gin.H{"ok": true, "skipped": true,
				"reason": "no watched paths changed"})
			return
		}
		if !known {
			slog.Info("watch_paths 已配置但本次推送未携带变更文件清单，照常部署", "app", app.ID)
		}
	}

	if h.engine.IsRunning(app.ID) {
		ev.Outcome = TriggerSkipped
		ev.Reason = "已有部署任务正在运行，跳过本次触发"
		h.recordTrigger(ev)
		c.JSON(http.StatusConflict, gin.H{"error": "another deployment is already running"})
		return
	}
	startedBy := actor
	if startedBy == "" {
		startedBy = "git-webhook"
	}
	dep, err := h.engine.StartDeployment(app, "webhook", startedBy)
	if err != nil {
		ev.Outcome = TriggerSkipped
		ev.Reason = "触发失败: " + err.Error()
		h.recordTrigger(ev)
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	ev.Outcome = TriggerTriggered
	ev.DeploymentID = dep.ID
	ev.Reason = fmt.Sprintf("Webhook 推送 %s 触发部署", branch)
	h.recordTrigger(ev)
	c.JSON(http.StatusOK, gin.H{"ok": true, "deployment": dep.ID})
}

func (h *AppHandlers) handleCIWebhook(c *gin.Context, app *Application, body []byte) {
	var payload struct {
		Tag   string `json:"tag"`
		Image string `json:"image"`
		Ref   string `json:"ref"`
		Actor string `json:"actor"`
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		_ = json.Unmarshal(body, &payload)
	}
	if qTag := strings.TrimSpace(c.Query("tag")); qTag != "" && payload.Tag == "" {
		payload.Tag = qTag
	}
	if qActor := strings.TrimSpace(c.Query("actor")); qActor != "" && payload.Actor == "" {
		payload.Actor = qActor
	}
	actor := strings.TrimSpace(payload.Actor)
	if actor == "" {
		actor = "ci-webhook"
	}
	ref := strings.TrimSpace(payload.Tag)
	if ref == "" {
		ref = strings.TrimSpace(payload.Ref)
	}
	ev := &TriggerEvent{
		AppID:  app.ID,
		Source: "ci",
		Ref:    ref,
		Actor:  actor,
	}

	if !app.AutoDeploy {
		ev.Outcome = TriggerRejected
		ev.Reason = "CI 自动触发部署未开启"
		h.recordTrigger(ev)
		c.JSON(http.StatusForbidden, gin.H{"error": "auto deploy disabled for this app"})
		return
	}

	nextApp := app
	if app.SourceType == "image" && (strings.TrimSpace(payload.Tag) != "" || strings.TrimSpace(payload.Image) != "") {
		resolved, err := resolveCIImageRef(app.Image, payload.Image, payload.Tag)
		if err != nil {
			ev.Outcome = TriggerRejected
			ev.Reason = err.Error()
			h.recordTrigger(ev)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if resolved != "" && resolved != app.Image {
			if err := h.store.UpdateApp(app.ID, func(a *Application) error {
				a.Image = resolved
				return nil
			}); err != nil {
				ev.Outcome = TriggerRejected
				ev.Reason = "更新目标镜像失败: " + err.Error()
				h.recordTrigger(ev)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			if fresh, ok := h.store.GetApp(app.ID); ok {
				nextApp = fresh
			}
		}
		ev.Ref = nextApp.Image
	}

	if h.engine.IsRunning(app.ID) {
		ev.Outcome = TriggerSkipped
		ev.Reason = "已有部署任务正在运行，跳过本次 CI 触发"
		h.recordTrigger(ev)
		c.JSON(http.StatusConflict, gin.H{"error": "another deployment is already running"})
		return
	}
	dep, err := h.engine.StartCIDeploy(nextApp, actor)
	if err != nil {
		ev.Outcome = TriggerSkipped
		ev.Reason = "触发失败: " + err.Error()
		h.recordTrigger(ev)
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	ev.Outcome = TriggerTriggered
	ev.DeploymentID = dep.ID
	if nextApp.Image != "" {
		ev.Reason = fmt.Sprintf("CI 触发拉取镜像 %s", nextApp.Image)
	} else {
		ev.Reason = "CI 触发 Compose 拉取与滚动更新"
	}
	h.recordTrigger(ev)
	c.JSON(http.StatusOK, gin.H{"ok": true, "deployment": dep.ID, "image": nextApp.Image})
}

// resolveCIImageRef ensures a CI webhook can promote a tag (or replace the same
// image's tag via a full reference) without redirecting the app to an arbitrary
// unrelated image repository.
func resolveCIImageRef(currentImage, reqImage, reqTag string) (string, error) {
	curBase, _ := splitImageRepoTag(currentImage)
	if curBase == "" {
		return "", fmt.Errorf("应用当前未配置基础镜像")
	}
	if ri := strings.TrimSpace(reqImage); ri != "" {
		reqBase, nextTag := splitImageRepoTag(ri)
		if reqBase != curBase {
			return "", fmt.Errorf("Webhook 仅允许更新同一镜像仓库（%s），拒绝切换到 %s", curBase, reqBase)
		}
		if nextTag != "" && !validImageTagRe.MatchString(nextTag) {
			return "", fmt.Errorf("镜像标签格式非法: %q", nextTag)
		}
		return ri, nil
	}
	tag := strings.TrimSpace(reqTag)
	if tag == "" {
		return currentImage, nil
	}
	if !validImageTagRe.MatchString(tag) {
		return "", fmt.Errorf("镜像标签格式非法: %q", tag)
	}
	return curBase + ":" + tag, nil
}

// splitImageRepoTag splits "ghcr.io/owner/app:v1" -> ("ghcr.io/owner/app", "v1"),
// preserving registry ports like "localhost:5000/app:v1".
func splitImageRepoTag(ref string) (repo, tag string) {
	s := strings.TrimSpace(ref)
	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[:at]
	}
	lastSlash := strings.LastIndexByte(s, '/')
	lastColon := strings.LastIndexByte(s, ':')
	if lastColon > lastSlash {
		return s[:lastColon], s[lastColon+1:]
	}
	return s, ""
}

func extractGitActorAndCommit(body []byte) (actor, commit string) {
	var p struct {
		After      string `json:"after"`
		HeadCommit *struct {
			ID string `json:"id"`
		} `json:"head_commit"`
		Pusher *struct {
			Name string `json:"name"`
		} `json:"pusher"`
		Sender *struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return "", ""
	}
	commit = strings.TrimSpace(p.After)
	if p.HeadCommit != nil && p.HeadCommit.ID != "" {
		commit = p.HeadCommit.ID
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}
	if p.Pusher != nil && p.Pusher.Name != "" {
		actor = p.Pusher.Name
	} else if p.Sender != nil && p.Sender.Login != "" {
		actor = p.Sender.Login
	}
	return actor, commit
}

func webhookBranch(c *gin.Context, body []byte) string {
	if ref := c.Query("ref"); ref != "" {
		return refFromRef(ref)
	}
	var payload struct {
		Ref        string `json:"ref"`
		RefName    string `json:"ref_name"`
		Repository *struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		Commits []struct {
			ID string `json:"id"`
		} `json:"commits"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if b := refFromRef(payload.Ref); b != "" {
		return b
	}
	if payload.RefName != "" {
		return strings.TrimPrefix(payload.RefName, "refs/heads/")
	}
	if payload.Branch != "" {
		return payload.Branch
	}
	if c.Query("force") == "1" {
		if payload.Repository != nil && payload.Repository.DefaultBranch != "" {
			return payload.Repository.DefaultBranch
		}
		return "main"
	}
	return ""
}

func refFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}
	return ref
}

func sameBranch(pushed, configured string) bool {
	if configured == "" {
		configured = "main"
	}
	return refFromRef(pushed) == refFromRef(configured)
}
