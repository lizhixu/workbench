package apps

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"context"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/rpc"
)

// AIDiagnoseFunc is a pluggable analyzer for failed deployment logs.
type AIDiagnoseFunc func(ctx context.Context, command, stdout, stderr string, exitCode int32) (any, error)

// AppHandlers manages Git-linked application CRUD, deployments and rollback.
type AppHandlers struct {
	reg         *rpc.Registry
	store       *Store
	engine      *Engine
	ai          AIDiagnoseFunc
	gitProvider GitWebhookManager
}

// GitWebhookManager handles automatic lifecycle sync for Git provider webhooks.
type GitWebhookManager interface {
	EnsureRepoWebhook(ctx context.Context, repoURL, webhookURL string) (int64, error)
	DeleteRepoWebhook(ctx context.Context, repoURL string, hookID int64) error
}

// NewAppHandlers creates the application management handlers.
func NewAppHandlers(reg *rpc.Registry, store *Store, engine *Engine) *AppHandlers {
	return &AppHandlers{reg: reg, store: store, engine: engine}
}

// SetAI injects the AI assistant for deployment failure diagnosis.
func (h *AppHandlers) SetAI(fn AIDiagnoseFunc) {
	h.ai = fn
}

// SetGitProvider injects the Git provider store for automatic Webhook lifecycle management.
func (h *AppHandlers) SetGitProvider(gp GitWebhookManager) {
	h.gitProvider = gp
}

// RegisterAppRoutes mounts the application lifecycle routes. Reads are open
// to every authenticated role; mutations (create/deploy/rollback/delete)
// require admin or operator and the caller wraps them with the audit
// middleware. The webhook endpoint is public but authenticated by its own
// random token, mirroring CI providers' webhook semantics.
func (h *AppHandlers) RegisterAppRoutes(readRG, writeRG, public *gin.RouterGroup) {
	readRG.GET("/apps", h.listApps)
	readRG.GET("/apps/:id", h.getApp)
	readRG.GET("/apps/:id/deployments", h.listDeployments)
	readRG.GET("/apps/:id/deployments/:depID", h.getDeployment)
	readRG.POST("/apps/:id/deployments/:depID/diagnose", h.diagnoseDeployment)
	readRG.GET("/apps/:id/status", h.appStatus)

	writeRG.POST("/apps", h.createApp)
	writeRG.PUT("/apps/:id", h.updateApp)
	writeRG.DELETE("/apps/:id", h.deleteApp)
	writeRG.POST("/apps/:id/deploy", h.deployApp)
	writeRG.POST("/apps/:id/rollback", h.rollbackApp)
	writeRG.POST("/apps/:id/stop", h.stopApp)
	writeRG.POST("/apps/:id/start", h.startApp)
	writeRG.POST("/apps/:id/restart", h.restartApp)
	writeRG.POST("/apps/:id/webhook/sync", h.syncAppWebhook)

	// Git provider webhooks: no JWT, token-in-path authentication.
	public.POST("/apps/webhook/:token", h.webhook)
	public.GET("/apps/webhook/:token", h.webhookGet)
}

func (h *AppHandlers) listApps(c *gin.Context) {
	apps := h.store.ListApps()
	c.JSON(http.StatusOK, gin.H{"data": apps})
}

func (h *AppHandlers) getApp(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": app})
}

type createAppReq struct {
	Name           string            `json:"name"`
	HostID         string            `json:"host_id"`
	SourceType     string            `json:"source_type"` // "git" | "raw_compose" | "image" | "template"
	RepoURL        string            `json:"repo_url"`
	Branch         string            `json:"branch"`
	AuthVaultID    string            `json:"auth_vault_id"`
	AutoDeploy     bool              `json:"auto_deploy"`
	BuildType      string            `json:"build_type"`
	Dockerfile     string            `json:"dockerfile"`
	BuildContext   string            `json:"build_context"`
	BuildTimeout   int32             `json:"build_timeout_sec"`
	ComposeContent string            `json:"compose_content"` // raw compose.yaml
	Image          string            `json:"image"`           // pre-built image for source_type=image/template
	TemplateID     string            `json:"template_id"`
	TemplateParams map[string]string `json:"template_params"`
	EnvVars        map[string]string `json:"env_vars"`
	Ports          []PortMapping     `json:"ports"`
	Volumes        []string          `json:"volumes"`
	HealthcheckURL string            `json:"healthcheck_url"`
	ContainerName  string            `json:"container_name"`
}

func (r *createAppReq) validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("应用名称不能为空")
	}
	if r.HostID == "" {
		return fmt.Errorf("必须选择目标主机")
	}

	srcType := r.SourceType
	if srcType == "" {
		srcType = "git"
	}

	switch srcType {
	case "raw_compose":
		if strings.TrimSpace(r.ComposeContent) == "" {
			return fmt.Errorf("Docker Compose 内容不能为空")
		}
	case "image":
		if strings.TrimSpace(r.Image) == "" {
			return fmt.Errorf("镜像地址不能为空")
		}
	case "template":
		if strings.TrimSpace(r.TemplateID) == "" {
			return fmt.Errorf("必须选择应用模板")
		}
	case "git":
		if strings.TrimSpace(r.RepoURL) == "" {
			return fmt.Errorf("Git 仓库地址不能为空")
		}
		if !strings.HasPrefix(r.RepoURL, "https://") && !strings.HasPrefix(r.RepoURL, "http://") {
			return fmt.Errorf("仅支持 http(s) Git 仓库地址")
		}
	default:
		return fmt.Errorf("不支持的部署来源类型: %s", srcType)
	}

	for k, v := range r.EnvVars {
		if k == "" || v == "" {
			return fmt.Errorf("环境变量名与值都不能为空")
		}
	}
	for _, pm := range r.Ports {
		if pm.Host <= 0 || pm.Host > 65535 || pm.Container <= 0 || pm.Container > 65535 {
			return fmt.Errorf("端口映射非法: %d:%d", pm.Host, pm.Container)
		}
	}
	return nil
}

func (h *AppHandlers) createApp(c *gin.Context) {
	var req createAppReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := req.validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.reg.Hub(req.HostID) == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "目标主机 Agent 不在线"})
		return
	}

	srcType := req.SourceType
	if srcType == "" {
		srcType = "git"
	}
	buildType := req.BuildType
	if buildType == "" {
		if srcType == "raw_compose" {
			buildType = "compose"
		} else {
			buildType = "dockerfile"
		}
	}

	app := &Application{
		ID:             "app_" + randomToken(8),
		Name:           strings.TrimSpace(req.Name),
		HostID:         req.HostID,
		SourceType:     srcType,
		RepoURL:        strings.TrimSpace(req.RepoURL),
		Branch:         firstNonEmpty(req.Branch, "main"),
		AuthVaultID:    req.AuthVaultID,
		AutoDeploy:     req.AutoDeploy,
		WebhookToken:   randomToken(16),
		BuildType:      buildType,
		Dockerfile:     firstNonEmpty(req.Dockerfile, "Dockerfile"),
		BuildContext:   firstNonEmpty(req.BuildContext, "."),
		BuildTimeout:   req.BuildTimeout,
		ComposeContent: req.ComposeContent,
		Image:          strings.TrimSpace(req.Image),
		TemplateID:     req.TemplateID,
		TemplateParams: req.TemplateParams,
		EnvVars:        req.EnvVars,
		Ports:          req.Ports,
		Volumes:        req.Volumes,
		HealthcheckURL: req.HealthcheckURL,
		ContainerName:  firstNonEmpty(req.ContainerName, "watchman-app-"+strings.ReplaceAll(strings.TrimSpace(req.Name), " ", "-")),
		CreatedAt:      time.Now(),
	}

	// If AutoDeploy is enabled for a GitHub repository and the Git Provider is connected,
	// automatically register the push webhook on the repository via GitHub API.
	if app.AutoDeploy && h.gitProvider != nil && strings.Contains(strings.ToLower(app.RepoURL), "github.com") {
		webhookURL := h.buildWebhookURL(c, app.WebhookToken)
		hookID, err := h.gitProvider.EnsureRepoWebhook(c.Request.Context(), app.RepoURL, webhookURL)
		if err == nil {
			app.WebhookAutoManaged = true
			app.GitHubHookID = hookID
			app.WebhookError = ""
		} else {
			app.WebhookAutoManaged = false
			app.WebhookError = err.Error()
		}
	}

	if err := h.store.PutApp(app); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": app})
}

func (h *AppHandlers) buildWebhookURL(c *gin.Context, token string) string {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := c.Request.Host
	if host == "" {
		host = "127.0.0.1:18080"
	}
	return fmt.Sprintf("%s://%s/api/v1/apps/webhook/%s", scheme, host, token)
}

func (h *AppHandlers) updateApp(c *gin.Context) {
	id := c.Param("id")
	oldApp, ok := h.store.GetApp(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	var req createAppReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.UpdateApp(id, func(a *Application) error {
		if strings.TrimSpace(req.Name) != "" {
			a.Name = strings.TrimSpace(req.Name)
		}
		if strings.TrimSpace(req.RepoURL) != "" {
			a.RepoURL = strings.TrimSpace(req.RepoURL)
		}
		if req.Branch != "" {
			a.Branch = req.Branch
		}
		a.AuthVaultID = req.AuthVaultID
		a.AutoDeploy = req.AutoDeploy
		if req.Dockerfile != "" {
			a.Dockerfile = req.Dockerfile
		}
		if req.BuildContext != "" {
			a.BuildContext = req.BuildContext
		}
		if req.BuildTimeout > 0 {
			a.BuildTimeout = req.BuildTimeout
		}
		if req.EnvVars != nil {
			a.EnvVars = req.EnvVars
		}
		if req.Ports != nil {
			a.Ports = req.Ports
		}
		if req.Volumes != nil {
			a.Volumes = req.Volumes
		}
		if req.ComposeContent != "" {
			a.ComposeContent = req.ComposeContent
		}
		if req.BuildType != "" {
			a.BuildType = req.BuildType
		}
		a.HealthcheckURL = req.HealthcheckURL

		// Sync webhook if auto_deploy state or repo changed
		if h.gitProvider != nil && strings.Contains(strings.ToLower(a.RepoURL), "github.com") {
			if a.AutoDeploy && (!oldApp.AutoDeploy || a.RepoURL != oldApp.RepoURL || a.GitHubHookID == 0) {
				webhookURL := h.buildWebhookURL(c, a.WebhookToken)
				hookID, err := h.gitProvider.EnsureRepoWebhook(c.Request.Context(), a.RepoURL, webhookURL)
				if err == nil {
					a.WebhookAutoManaged = true
					a.GitHubHookID = hookID
					a.WebhookError = ""
				} else {
					a.WebhookAutoManaged = false
					a.WebhookError = err.Error()
				}
			} else if !a.AutoDeploy && oldApp.AutoDeploy && oldApp.GitHubHookID > 0 {
				_ = h.gitProvider.DeleteRepoWebhook(c.Request.Context(), oldApp.RepoURL, oldApp.GitHubHookID)
				a.WebhookAutoManaged = false
				a.GitHubHookID = 0
				a.WebhookError = ""
			}
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	app, _ := h.store.GetApp(id)
	c.JSON(http.StatusOK, gin.H{"data": app})
}

func (h *AppHandlers) deleteApp(c *gin.Context) {
	id := c.Param("id")
	purge := c.Query("purge") == "true" || c.Query("purge") == "1"
	app, ok := h.store.GetApp(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	var warnings []string
	if purge {
		var err error
		warnings, err = h.purgeAppResources(app)
		if err != nil {
			// Agent offline etc: refuse so resources are not silently orphaned.
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
	}
	if app.WebhookAutoManaged && app.GitHubHookID > 0 && h.gitProvider != nil {
		_ = h.gitProvider.DeleteRepoWebhook(context.Background(), app.RepoURL, app.GitHubHookID)
	}
	if err := h.store.DeleteApp(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	resp := gin.H{"ok": true}
	if purge {
		resp["purged"] = true
		if len(warnings) > 0 {
			resp["warnings"] = warnings
		}
	}
	c.JSON(http.StatusOK, resp)
}

func (h *AppHandlers) syncAppWebhook(c *gin.Context) {
	id := c.Param("id")
	app, ok := h.store.GetApp(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "应用不存在"})
		return
	}
	if h.gitProvider == nil || !strings.Contains(strings.ToLower(app.RepoURL), "github.com") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该应用不是 GitHub 仓库或未启用 Git Provider 集成"})
		return
	}
	webhookURL := h.buildWebhookURL(c, app.WebhookToken)
	hookID, err := h.gitProvider.EnsureRepoWebhook(c.Request.Context(), app.RepoURL, webhookURL)
	if err != nil {
		_ = h.store.UpdateApp(id, func(a *Application) error {
			a.WebhookAutoManaged = false
			a.WebhookError = err.Error()
			return nil
		})
		c.JSON(http.StatusBadGateway, gin.H{"error": "自动同步 GitHub Webhook 失败: " + err.Error()})
		return
	}
	_ = h.store.UpdateApp(id, func(a *Application) error {
		a.WebhookAutoManaged = true
		a.GitHubHookID = hookID
		a.WebhookError = ""
		return nil
	})
	updated, _ := h.store.GetApp(id)
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": "已成功通过 GitHub API 为该仓库配置 Webhook",
		"data":    updated,
	})
}

func (h *AppHandlers) deployApp(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	dep, err := h.engine.StartDeployment(app, "manual", usernameOf(c))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": dep})
}

// rollbackApp re-deploys a historical version. For git-backed apps it
// re-runs the pipeline pinned to the recorded commit (checkout after fetch,
// so the rollback goes through the same audited path as a normal deploy).
// For raw_compose apps there is no git history; rollback restores the
// compose.yaml snapshot recorded on the target deployment.
func (h *AppHandlers) rollbackApp(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	depID := c.Query("deployment_id")
	if depID == "" {
		// Roll back to the previous successful deployment.
		success := h.store.ListSuccessfulDeployments(app.ID)
		if len(success) < 2 || success[0].ID == "" {
			c.JSON(http.StatusConflict, gin.H{"error": "没有可回滚的历史版本"})
			return
		}
		depID = success[1].ID
	}
	target, ok := h.store.GetDeployment(app.ID, depID)
	if !ok || target.Status != DeploySuccess {
		c.JSON(http.StatusConflict, gin.H{"error": "目标部署记录不存在或未成功"})
		return
	}
	pinned := *app
	pinned.Branch = app.Branch
	if app.SourceType == "raw_compose" {
		if target.ComposeContent == "" {
			c.JSON(http.StatusConflict, gin.H{"error": "该历史版本没有 Compose 内容快照，无法回滚（仅支持此次更新之后的部署）"})
			return
		}
		pinned.ComposeContent = target.ComposeContent
		dep, err := h.engine.StartRollback(&pinned, "", usernameOf(c))
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": dep})
		return
	}
	if target.CommitHash == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "该部署记录缺少 Commit 信息，无法回滚"})
		return
	}
	// Check out the recorded commit after fetch: simplest reliable path is a
	// deploy whose fetch script additionally pins to the target commit.
	dep, err := h.engine.StartRollback(&pinned, target.CommitHash, usernameOf(c))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": dep})
}

// diagnoseDeployment uses the AI engine to analyze a failed deployment log.
func (h *AppHandlers) diagnoseDeployment(c *gin.Context) {
	if h.ai == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "AI 助手未启用或未配置"})
		return
	}
	appID := c.Param("id")
	depID := c.Param("depID")
	dep, ok := h.store.GetDeployment(appID, depID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment not found"})
		return
	}
	if dep.Status != DeployFailed && dep.Error == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅支持对失败的部署记录进行智能排障"})
		return
	}

	cmd := "docker build / git deploy"
	stdout := dep.BuildLog
	stderr := dep.Error
	res, err := h.ai(c.Request.Context(), cmd, stdout, stderr, dep.ExitCode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "AI 诊断失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func (h *AppHandlers) stopApp(c *gin.Context) {
	h.containerAction(c, "stop")
}

func (h *AppHandlers) startApp(c *gin.Context) {
	h.containerAction(c, "start")
}

func (h *AppHandlers) restartApp(c *gin.Context) {
	h.containerAction(c, "restart")
}

func (h *AppHandlers) containerAction(c *gin.Context, op string) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	hub := h.reg.Hub(app.HostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}
	container := app.ContainerName
	if container == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "应用尚未部署过"})
		return
	}
	res, err := execOnAgent(hub, "app-ctl-"+randomToken(4), "docker "+op+" "+shellQuote(container), 60)
	if err != nil {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": res.GetExitCode() == 0, "output": string(res.GetStdout())})
}

func (h *AppHandlers) listDeployments(c *gin.Context) {
	offset := intQuery(c, "offset", 0)
	pageSize := intQuery(c, "page_size", 20)
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	list, total := h.store.ListDeployments(c.Param("id"), offset, pageSize)
	if list == nil {
		list = []*Deployment{}
	}
	c.JSON(http.StatusOK, gin.H{"data": list, "total": total, "offset": offset, "page_size": pageSize})
}

func (h *AppHandlers) getDeployment(c *gin.Context) {
	dep, ok := h.store.GetDeployment(c.Param("id"), c.Param("depID"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": dep})
}

// appStatus returns the live container state of the app on its host.
func (h *AppHandlers) appStatus(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	resp := gin.H{
		"app_id":         app.ID,
		"deploying":      h.engine.IsRunning(app.ID),
		"current_commit": app.CurrentCommit,
	}
	hub := h.reg.Hub(app.HostID)
	if hub == nil {
		resp["agent_online"] = false
		resp["state"] = "unknown"
		c.JSON(http.StatusOK, gin.H{"data": resp})
		return
	}
	resp["agent_online"] = true
	container := app.ContainerName
	if container == "" {
		resp["state"] = "undeployed"
		c.JSON(http.StatusOK, gin.H{"data": resp})
		return
	}
	res, err := execOnAgent(hub, "app-stat-"+randomToken(4),
		fmt.Sprintf("docker inspect --format '{{.State.Status}}|{{.State.StartedAt}}' %s 2>/dev/null || echo 'missing'", shellQuote(container)), 20)
	if err != nil {
		resp["state"] = "unknown"
		c.JSON(http.StatusOK, gin.H{"data": resp})
		return
	}
	fields := strings.Split(strings.TrimSpace(string(res.GetStdout())), "|")
	if len(fields) > 0 && fields[0] != "" {
		resp["state"] = fields[0]
	}
	if len(fields) > 1 {
		resp["started_at"] = fields[1]
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

func usernameOf(c *gin.Context) string {
	if v, ok := c.Get("username"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func intQuery(c *gin.Context, name string, def int) int {
	raw := c.Query(name)
	if raw == "" {
		return def
	}
	n := 0
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
		if n > 1_000_000 {
			return def
		}
	}
	return n
}
