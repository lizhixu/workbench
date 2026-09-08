package apps

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// webhook handles Git provider push events. Authentication is the per-app
// random token in the path, mirroring how Dokploy/Coolify webhook URLs work;
// no JWT because the caller is a Git provider's server. Branch filtering
// supports GitHub, GitLab and Gitee push payload shapes (plus the generic
// `ref` query param used by plain curl triggers).
func (h *AppHandlers) webhook(c *gin.Context) {
	h.handleWebhook(c, c.Param("token"))
}

// webhookGet supports `curl`-style trigger URLs; providers like some CI
// systems send GET pings.
func (h *AppHandlers) webhookGet(c *gin.Context) {
	h.handleWebhook(c, c.Param("token"))
}

func (h *AppHandlers) handleWebhook(c *gin.Context, token string) {
	app, ok := h.store.FindAppByWebhookToken(token)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown webhook token"})
		return
	}
	if !app.AutoDeploy {
		c.JSON(http.StatusForbidden, gin.H{"error": "auto deploy disabled for this app"})
		return
	}

	// GitHub (and some others) deliver JSON inside a form field; parse that
	// BEFORE reading the raw body, since form parsing consumes the body.
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
	branch := webhookBranch(c, body)
	if branch == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot determine pushed branch"})
		return
	}
	if !sameBranch(branch, app.Branch) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "skipped": true,
			"reason": "branch does not match " + app.Branch})
		return
	}

	if h.engine.IsRunning(app.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "another deployment is already running"})
		return
	}
	dep, err := h.engine.StartDeployment(app, "webhook", "git-webhook")
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "deployment": dep.ID})
}

// webhookBranch extracts the pushed branch name from the common provider
// payload shapes.
func webhookBranch(c *gin.Context, body []byte) string {
	if ref := c.Query("ref"); ref != "" {
		return refFromRef(ref)
	}
	var payload struct {
		Ref        string `json:"ref"`      // GitHub/Gitee/GitLab: "refs/heads/main"
		RefName    string `json:"ref_name"` // Gitee alt
		Repository *struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		// GitLab push event.
		Commits []struct {
			ID string `json:"id"`
		} `json:"commits"`
		// Generic tag push (Dokploy-style) has no ref; some send just branch.
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
	// Some providers ping webhooks with no ref (e.g. a GitHub "ping" event).
	// Treat that as a manual trigger on the default branch when the query
	// explicitly allows it (?force=1), otherwise refuse rather than guess.
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

// sameBranch compares branch names tolerantly ("main" vs "refs/heads/main").
func sameBranch(pushed, configured string) bool {
	if configured == "" {
		configured = "main"
	}
	return refFromRef(pushed) == refFromRef(configured)
}
