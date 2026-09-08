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
	reg    *rpc.Registry
	store  *Store
	engine *Engine
	ai     AIDiagnoseFunc
}

// NewAppHandlers creates the application management handlers.
func NewAppHandlers(reg *rpc.Registry, store *Store, engine *Engine) *AppHandlers {
	return &AppHandlers{reg: reg, store: store, engine: engine}
}

// SetAI injects the AI assistant for deployment failure diagnosis.
func (h *AppHandlers) SetAI(fn AIDiagnoseFunc) {
	h.ai = fn
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
	SourceType     string            `json:"source_type"` // "git" | "raw_compose"
	RepoURL        string            `json:"repo_url"`
	Branch         string            `json:"branch"`
	AuthVaultID    string            `json:"auth_vault_id"`
	AutoDeploy     bool              `json:"auto_deploy"`
	BuildType      string            `json:"build_type"`
	Dockerfile     string            `json:"dockerfile"`
	BuildContext   string            `json:"build_context"`
	BuildTimeout   int32             `json:"build_timeout_sec"`
	ComposeContent string            `json:"compose_content"` // raw compose.yaml
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
	if r.SourceType == "raw_compose" {
		if strings.TrimSpace(r.ComposeContent) == "" {
			return fmt.Errorf("Docker Compose 内容不能为空")
		}
		return nil
	}
	if strings.TrimSpace(r.RepoURL) == "" {
		return fmt.Errorf("Git 仓库地址不能为空")
	}
	if !strings.HasPrefix(r.RepoURL, "https://") && !strings.HasPrefix(r.RepoURL, "http://") {
		return fmt.Errorf("仅支持 http(s) Git 仓库地址")
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
		EnvVars:        req.EnvVars,
		Ports:          req.Ports,
		Volumes:        req.Volumes,
		HealthcheckURL: req.HealthcheckURL,
		ContainerName:  firstNonEmpty(req.ContainerName, "watchman-app-"+strings.ReplaceAll(strings.TrimSpace(req.Name), " ", "-")),
		CreatedAt:      time.Now(),
	}
	if err := h.store.PutApp(app); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": app})
}

func (h *AppHandlers) updateApp(c *gin.Context) {
	id := c.Param("id")
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
	if err := h.store.DeleteApp(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
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

// rollbackApp re-deploys a historical successful commit. Rather than
// rebuilding from the old source, it re-runs the pipeline pinned to the
// recorded commit; the engine's fetch step always checks out the branch
// head, so rollback passes the target commit through the same path for
// consistency and auditability.
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
	if target.CommitHash == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "该部署记录缺少 Commit 信息，无法回滚"})
		return
	}
	// Check out the recorded commit after fetch: simplest reliable path is a
	// deploy whose fetch script additionally pins to the target commit.
	pinned := *app
	pinned.Branch = app.Branch
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
	res, err := execOnAgent(hub, "app-ctl-"+randomToken(4), "docker "+op+" "+container, 60)
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
		fmt.Sprintf("docker inspect --format '{{.State.Status}}|{{.State.StartedAt}}' %s 2>/dev/null || echo 'missing'", container), 20)
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
