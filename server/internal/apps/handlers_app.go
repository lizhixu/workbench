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

	imageInspector ImageInspectFunc
	repoInspector  RepoInspectFunc
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
	// App volume backup/restore: archive listing is a read (the download itself
	// goes through the existing host file endpoint).
	readRG.GET("/apps/:id/volume-backups", h.listVolumeBackups)

	// Image / repository metadata probing: read-only ground-truth fetchers used
	// by the create-form auto-fill hints and the AI app assistant. No state is
	// mutated, so these stay on the read group without operation audit.
	readRG.POST("/apps/inspect-image", h.inspectImage)
	readRG.POST("/apps/inspect-repo", h.inspectRepo)

	writeRG.POST("/apps", h.createApp)
	writeRG.PUT("/apps/:id", h.updateApp)
	writeRG.DELETE("/apps/:id", h.deleteApp)
	writeRG.POST("/apps/:id/deploy", h.deployApp)
	writeRG.POST("/apps/:id/deployments/cancel", h.cancelDeployment)
	writeRG.POST("/apps/:id/rollback", h.rollbackApp)
	writeRG.POST("/apps/:id/stop", h.stopApp)
	writeRG.POST("/apps/:id/start", h.startApp)
	writeRG.POST("/apps/:id/restart", h.restartApp)
	writeRG.POST("/apps/:id/webhook/sync", h.syncAppWebhook)
	// Volume backup/restore mutates host data (tar over live volume dirs) and
	// a restore stops/restarts the app container, so these ride the audited
	// write group.
	writeRG.POST("/apps/:id/volume-backups", h.createVolumeBackup)
	writeRG.POST("/apps/:id/volume-backups/:archiveID/restore", h.restoreVolumeBackup)
	writeRG.DELETE("/apps/:id/volume-backups/:archiveID", h.deleteVolumeBackup)

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
	SourceType     string            `json:"source_type"` // "git" | "raw_compose" | "image" | "template" | "forward"
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
		// Mesh Forwarding (TCP Relay via an entry host)
		MeshForwardEnabled    bool   `json:"mesh_forward_enabled"`
		MeshForwardHostID     string `json:"mesh_forward_host_id"`
		MeshForwardEntryPort  int    `json:"mesh_forward_entry_port"`
		MeshForwardTargetPort int    `json:"mesh_forward_target_port"`
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
	if r.MeshForwardEnabled {
		if err := validateMeshForward(r.HostID, r.MeshForwardEnabled, r.MeshForwardHostID, r.MeshForwardEntryPort, r.MeshForwardTargetPort); err != nil {
			return err
		}
	}
	return nil
}

// validateMeshForward enforces the entry-host relay rules shared by create
// and update: hostID is the app's own deploy host (immutable on update).
func validateMeshForward(appHostID string, enabled bool, entryHostID string, entryPort, targetPort int) error {
	if !enabled {
		return nil
	}
	if strings.TrimSpace(entryHostID) == "" {
		return fmt.Errorf("启用组网转发时必须选择入口主机")
	}
	if entryHostID == appHostID {
		return fmt.Errorf("组网转发入口主机不能与应用部署主机相同")
	}
	if entryPort <= 0 || entryPort > 65535 {
		return fmt.Errorf("组网转发入口端口非法: %d", entryPort)
	}
	if targetPort <= 0 || targetPort > 65535 {
		return fmt.Errorf("组网转发目标端口非法: %d", targetPort)
	}
	return nil
}

// meshForwardConflict enforces one relay per (entry host, entry port): two
// apps racing the same bind would only fail inside a discarded agent log
// line, so the collision is rejected up front with a 409.
func (h *AppHandlers) meshForwardConflict(appID, entryHostID string, entryPort int) error {
	for _, a := range h.store.ListApps() {
		if a.ID == appID || !a.MeshForwardEnabled {
			continue
		}
		if a.MeshForwardHostID == entryHostID && a.MeshForwardEntryPort == entryPort {
			return fmt.Errorf("入口主机端口 %d 已被应用「%s」的组网转发占用", entryPort, a.Name)
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

		if req.MeshForwardEnabled {
			if h.reg.Hub(req.MeshForwardHostID) == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "组网转发入口主机 Agent 不在线"})
				return
			}
			if err := h.meshForwardConflict("", req.MeshForwardHostID, req.MeshForwardEntryPort); err != nil {
				c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
				return
			}
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
		containerName := firstNonEmpty(req.ContainerName, "watchman-app-"+strings.ReplaceAll(strings.TrimSpace(req.Name), " ", "-"))
		image := strings.TrimSpace(req.Image)

		app := &Application{
			ID:                    "app_" + randomToken(8),
			Name:                  strings.TrimSpace(req.Name),
			HostID:                req.HostID,
			SourceType:            srcType,
			RepoURL:               strings.TrimSpace(req.RepoURL),
			Branch:                firstNonEmpty(req.Branch, "main"),
			AuthVaultID:           req.AuthVaultID,
			AutoDeploy:            req.AutoDeploy,
			WebhookToken:          randomToken(16),
			BuildType:             buildType,
			Dockerfile:            firstNonEmpty(req.Dockerfile, "Dockerfile"),
			BuildContext:          firstNonEmpty(req.BuildContext, "."),
			BuildTimeout:          req.BuildTimeout,
			ComposeContent:        req.ComposeContent,
			Image:                 image,
			TemplateID:            req.TemplateID,
			TemplateParams:        req.TemplateParams,
			EnvVars:               req.EnvVars,
			MeshForwardEnabled:    req.MeshForwardEnabled,
			MeshForwardHostID:     req.MeshForwardHostID,
			MeshForwardEntryPort:  req.MeshForwardEntryPort,
			MeshForwardTargetPort: req.MeshForwardTargetPort,
			Ports:                 req.Ports,
			Volumes:               req.Volumes,
			HealthcheckURL:        req.HealthcheckURL,
			ContainerName:         containerName,
			CreatedAt:             time.Now(),
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
	// Creation is the first deployment: kick it off right away so a new app
	// goes from form to running container without a second click. The engine
	// returns as soon as the record is queued — the front-end already polls
	// deploy status on the detail page. A queue/engine failure must not fail
	// the create itself (the app record is valid and deploy can be retried).
	resp := gin.H{"data": app}
	if h.engine != nil {
		dep, err := h.engine.StartDeployment(app, "create", usernameOf(c))
		if err != nil {
			resp["deploy_warning"] = "应用已创建，但自动部署未能启动: " + err.Error()
		} else {
			resp["deployment"] = dep
		}
	}
	c.JSON(http.StatusOK, resp)
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

// updateAppReq is the body of PUT /apps/:id. Every mutable field is a pointer
// so an omitted key means "leave unchanged". The previous code reused
// createAppReq, whose non-pointer fields were overwritten unconditionally:
// saving only the env vars silently wiped auto_deploy, the Git credential,
// the health check URL and a running mesh relay. Explicit false / "" / {}
// still clears a field; nil means untouched.
//
// host_id, source_type and container_name stay immutable here (moving an app
// to another host is a redeploy, not an edit).
type updateAppReq struct {
	Name        *string `json:"name"`
	RepoURL     *string `json:"repo_url"`
	Branch      *string `json:"branch"`
	AuthVaultID *string `json:"auth_vault_id"`
	AutoDeploy  *bool   `json:"auto_deploy"`

	BuildType      *string `json:"build_type"`
	Dockerfile     *string `json:"dockerfile"`
	BuildContext   *string `json:"build_context"`
	BuildTimeout   *int32  `json:"build_timeout_sec"`
	ComposeContent *string `json:"compose_content"`
	Image          *string `json:"image"`

	EnvVars        *map[string]string `json:"env_vars"`
	Ports          *[]PortMapping     `json:"ports"`
	Volumes        *[]string          `json:"volumes"`
	HealthcheckURL *string            `json:"healthcheck_url"`

	MeshForwardEnabled    *bool   `json:"mesh_forward_enabled"`
	MeshForwardHostID     *string `json:"mesh_forward_host_id"`
	MeshForwardEntryPort  *int    `json:"mesh_forward_entry_port"`
	MeshForwardTargetPort *int    `json:"mesh_forward_target_port"`
}

func (h *AppHandlers) updateApp(c *gin.Context) {
	id := c.Param("id")
	oldApp, ok := h.store.GetApp(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	var req updateAppReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "应用名称不能为空"})
		return
	}
	if req.EnvVars != nil {
		for k, v := range *req.EnvVars {
			if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "环境变量名与值都不能为空"})
				return
			}
		}
	}
	if req.Image != nil {
		img := strings.TrimSpace(*req.Image)
		if img == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "镜像地址不能为空"})
			return
		}
		// Template images are derived from the catalog and Git/compose supply
		// their own build output; only a pre-built image app can retag.
		if oldApp.SourceType != "image" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "仅「镜像」来源的应用支持修改镜像地址"})
			return
		}
		*req.Image = img
	}
	if req.Ports != nil {
		for _, pm := range *req.Ports {
			if pm.Host <= 0 || pm.Host > 65535 || pm.Container <= 0 || pm.Container > 65535 {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("端口映射非法: %d:%d", pm.Host, pm.Container)})
				return
			}
		}
	}

	// Mesh-forward rules are validated on the EFFECTIVE config (stored values
	// with this request applied), and only when the request touches a relay
	// field — an unrelated edit must not fail because the entry host went
	// offline since the relay was configured.
	meshTouched := req.MeshForwardEnabled != nil || req.MeshForwardHostID != nil ||
		req.MeshForwardEntryPort != nil || req.MeshForwardTargetPort != nil
	effMeshEnabled := oldApp.MeshForwardEnabled
	effMeshHost := oldApp.MeshForwardHostID
	effEntryPort := oldApp.MeshForwardEntryPort
	effTargetPort := oldApp.MeshForwardTargetPort
	if req.MeshForwardEnabled != nil {
		effMeshEnabled = *req.MeshForwardEnabled
	}
	if req.MeshForwardHostID != nil {
		effMeshHost = strings.TrimSpace(*req.MeshForwardHostID)
	}
	if req.MeshForwardEntryPort != nil {
		effEntryPort = *req.MeshForwardEntryPort
	}
	if req.MeshForwardTargetPort != nil {
		effTargetPort = *req.MeshForwardTargetPort
	}
	if meshTouched {
		// host_id is immutable here, so validate against the existing app host.
		if err := validateMeshForward(oldApp.HostID, effMeshEnabled, effMeshHost, effEntryPort, effTargetPort); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if effMeshEnabled {
			if h.reg.Hub(effMeshHost) == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "组网转发入口主机 Agent 不在线"})
				return
			}
			if err := h.meshForwardConflict(id, effMeshHost, effEntryPort); err != nil {
				c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
				return
			}
		}
	}

	if err := h.store.UpdateApp(id, func(a *Application) error {
		if req.Name != nil {
			a.Name = strings.TrimSpace(*req.Name)
		}
		if req.RepoURL != nil {
			a.RepoURL = strings.TrimSpace(*req.RepoURL)
		}
		if req.Branch != nil {
			a.Branch = strings.TrimSpace(*req.Branch)
		}
		if req.AuthVaultID != nil {
			a.AuthVaultID = *req.AuthVaultID
		}
		if req.AutoDeploy != nil {
			a.AutoDeploy = *req.AutoDeploy
		}
		if req.Dockerfile != nil && *req.Dockerfile != "" {
			a.Dockerfile = *req.Dockerfile
		}
		if req.BuildContext != nil && *req.BuildContext != "" {
			a.BuildContext = *req.BuildContext
		}
		if req.BuildTimeout != nil && *req.BuildTimeout > 0 {
			a.BuildTimeout = *req.BuildTimeout
		}
		if req.EnvVars != nil {
			a.EnvVars = *req.EnvVars
		}
		if req.Ports != nil {
			a.Ports = *req.Ports
		}
		if req.Volumes != nil {
			a.Volumes = *req.Volumes
		}
		if req.ComposeContent != nil && *req.ComposeContent != "" {
			a.ComposeContent = *req.ComposeContent
		}
		if req.Image != nil {
			a.Image = *req.Image
		}
		if req.BuildType != nil && *req.BuildType != "" {
			a.BuildType = *req.BuildType
		}
		if req.HealthcheckURL != nil {
			a.HealthcheckURL = strings.TrimSpace(*req.HealthcheckURL)
		}
		if req.MeshForwardEnabled != nil {
			a.MeshForwardEnabled = *req.MeshForwardEnabled
		}
		if req.MeshForwardHostID != nil {
			a.MeshForwardHostID = strings.TrimSpace(*req.MeshForwardHostID)
		}
		if req.MeshForwardEntryPort != nil {
			a.MeshForwardEntryPort = *req.MeshForwardEntryPort
		}
		if req.MeshForwardTargetPort != nil {
			a.MeshForwardTargetPort = *req.MeshForwardTargetPort
		}
		if !a.MeshForwardEnabled {
			// A disabled relay keeps no coordinates: leaving them behind makes
			// the record point at a container that purge would then chase.
			a.MeshForwardHostID = ""
			a.MeshForwardEntryPort = 0
			a.MeshForwardTargetPort = 0
			a.MeshForwardTarget = ""
		}

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
	resp := gin.H{"data": app}
	if app != nil && h.engine != nil {
		meshChanged := oldApp.MeshForwardEnabled != app.MeshForwardEnabled ||
			oldApp.MeshForwardHostID != app.MeshForwardHostID ||
			oldApp.MeshForwardEntryPort != app.MeshForwardEntryPort ||
			oldApp.MeshForwardTargetPort != app.MeshForwardTargetPort
		if meshChanged {
			// The stored fields were overwritten/cleared: the old relay would
			// otherwise be orphaned with nothing in the record pointing at it,
			// and purge would never find it. Tear it down by its old location.
			if oldApp.MeshForwardHostID != "" && oldApp.MeshForwardHostID != app.MeshForwardHostID {
				h.engine.CleanMeshForwarder(oldApp.MeshForwardHostID, id)
			}
			deployed := app.ContainerName != "" || app.CurrentCommit != "" || app.SourceType == "raw_compose"
			if deployed && app.MeshForwardEnabled {
				// Synchronous so the user sees the relay outcome in the
				// update response instead of a discarded goroutine error.
				target, err := h.engine.SyncMeshForwarder(app)
				switch {
				case err != nil:
					resp["mesh_forward_warning"] = "组网转发中继同步失败: " + err.Error()
				case target != "":
					resp["mesh_forward_target"] = target
				}
			}
		}
	}
	c.JSON(http.StatusOK, resp)
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
	var req struct {
		Force bool `json:"force"`
	}
	_ = c.ShouldBindJSON(&req)
	force := req.Force || c.Query("force") == "true" || c.Query("force") == "1"

	if force && h.engine != nil {
		h.engine.CancelDeployment(app.ID, usernameOf(c))
	}

	dep, err := h.engine.StartDeployment(app, "manual", usernameOf(c))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": dep})
}

func (h *AppHandlers) cancelDeployment(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment engine unavailable"})
		return
	}
	cancelled := h.engine.CancelDeployment(app.ID, usernameOf(c))
	msg := "当前没有正在执行的部署任务"
	if cancelled {
		msg = "已成功终止部署任务"
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":        true,
		"cancelled": cancelled,
		"message":   msg,
	})
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
		resp := gin.H{"ok": res.GetExitCode() == 0, "output": string(res.GetStdout())}
		if app.MeshForwardEnabled && app.MeshForwardHostID != "" {
			fwdHub := h.reg.Hub(app.MeshForwardHostID)
			if fwdHub == nil {
				resp["relay_warning"] = "组网转发入口主机离线，中继容器未同步操作"
			} else {
				fwdContainer := "watchman-fwd-" + app.ID
				fres, ferr := execOnAgent(fwdHub, "fwd-ctl-"+randomToken(4), "docker "+op+" "+shellQuote(fwdContainer), 30)
				switch {
				case ferr != nil:
					resp["relay_warning"] = "中继容器 " + op + " 失败: " + ferr.Error()
				case fres.GetExitCode() != 0:
					resp["relay_warning"] = fmt.Sprintf("中继容器 %s 失败(exit %d): %s", op, fres.GetExitCode(), strings.TrimSpace(string(fres.GetStderr())))
				}
			}
		}
		c.JSON(http.StatusOK, resp)
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
