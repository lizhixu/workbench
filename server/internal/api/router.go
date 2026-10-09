// Package api implements the control server's REST API.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"watchman/internal/binval"
	"watchman/internal/version"
	"watchman/proto/agentpb"
	"watchman/server/internal/ai"
	"watchman/server/internal/alert"
	"watchman/server/internal/apps"
	"watchman/server/internal/audit"
	"watchman/server/internal/auth"
	"watchman/server/internal/backup"
	"watchman/server/internal/cert"
	"watchman/server/internal/commands"
	"watchman/server/internal/gitprovider"
	"watchman/server/internal/groups"
	"watchman/server/internal/install"
	"watchman/server/internal/metrics"
	"watchman/server/internal/network"
	"watchman/server/internal/panelsec"
	"watchman/server/internal/policy"
	"watchman/server/internal/release"
	"watchman/server/internal/rpc"
	"watchman/server/internal/scan"
	"watchman/server/internal/secentry"
	"watchman/server/internal/session"
	"watchman/server/internal/settings"
	"watchman/server/internal/snapshots"
	"watchman/server/internal/vault"
	"watchman/server/internal/ws"
	"watchman/server/web"

	"github.com/gin-gonic/gin"
)

// CurrentAgentVersion is the fallback agent target version, used only when
// no release manifest is deployed (dev builds). In production install.sh
// deploys /opt/watchman/manifest.json and api.SetAgentRelease makes the
// manifest the source of truth — see agentTargetVersion in release.go.
// It comes from the linker (see internal/version) so releasing a new agent
// only requires rebuilding with -ldflags, never editing a hardcoded string.
var CurrentAgentVersion = version.Get()

// HostDTO is the public representation of a managed host.
type HostDTO struct {
	ID         string   `json:"id"`
	Hostname   string   `json:"hostname"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	Distro     string   `json:"distro"`
	Version    string   `json:"agent_version"`
	Status     string   `json:"status"`
	LastSeen   string   `json:"last_seen"`
	Registered string   `json:"registered"`
	Group      string   `json:"group"`
	Tags       []string `json:"tags"`
	Uptime     int64    `json:"uptime"`
	CPUCores   int32    `json:"cpu_cores"`
	MemTotal   int64    `json:"mem_total"`
	InternalIP string   `json:"internal_ip"`
	PublicIP   string   `json:"public_ip"`
	Location   string   `json:"location"`
	// Optional billing & traffic quota configurations
	Price           float64 `json:"price,omitempty"`
	Currency        string  `json:"currency,omitempty"`
	BillingCycle    string  `json:"billing_cycle,omitempty"`
	ExpiresAt       string  `json:"expires_at,omitempty"`
	AutoRenewal     bool    `json:"auto_renewal,omitempty"`
	TrafficLimitGB  float64 `json:"traffic_limit_gb,omitempty"`
	TrafficCalcType string  `json:"traffic_calc_type,omitempty"`
	TrafficResetDay int     `json:"traffic_reset_day,omitempty"`
	RenewalURL      string  `json:"renewal_url,omitempty"`
	Notes           string  `json:"notes,omitempty"`
	// Extended live metrics (from the agent's latest sample).
	CpuModel  string  `json:"cpu_model,omitempty"`
	Load1     float64 `json:"load1"`
	SwapUsage float64 `json:"swap_usage"`
	MonthRx   int64   `json:"month_rx"`
	MonthTx   int64   `json:"month_tx"`
	// In-progress upgrade tracking (survives page refreshes until reconnected or timeout).
	Upgrading          bool   `json:"upgrading"`
	UpgradeStage       string `json:"upgrade_stage,omitempty"`
	UpgradeTarget      string `json:"upgrade_target,omitempty"`
	UpgradeError       string `json:"upgrade_error,omitempty"`
	AgentLatestVersion string `json:"agent_latest_version,omitempty"`
	AgentOutdated      bool   `json:"agent_outdated"`
}

// Router builds the gin engine with all routes mounted under /api/v1.
// authStore may be nil for a degraded (token-less) mode used only in tests;
// in production it is always provided. sessStore tracks terminal sessions
// for audit (may be nil to disable auditing). auditStore records the unified
// operational audit trail (may be nil to disable). commandStore backs the
// saved-command library, groupStore backs host grouping + per-group user
// authorization, prefsStore backs per-user terminal preferences and
// backupStore backs control-plane backup/restore; all may be nil to disable
// those features.
func Router(reg *rpc.Registry, log *slog.Logger, authStore *auth.Store, sessStore *session.Store, alertStore *alert.Store, vaultStore *vault.Store, aiAssistant *ai.Assistant, metricsStore *metrics.Store, scanStore *scan.Store, policyStore *policy.Store, auditStore *audit.Store, commandStore *commands.Store, groupStore *groups.Store, settingsStore *settings.Store, backupStore *backup.Store, networkStore *network.Store, appStore *apps.Store, appEngine *apps.Engine, certHub *cert.Hub, snapshotStore *snapshots.Store, snapshotEngine *snapshots.Engine, gitProviderStore *gitprovider.Store, alertMon *alert.Monitor) *gin.Engine {
	if log == nil {
		log = slog.Default()
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), requestLogger(log))

	// Secure entry (安全入口): once enabled in system settings, the panel can
	// only be logged in through the secret entry URL. The guard 404s direct
	// page/API access without a valid JWT or entry cookie; agent
	// install/enroll, webhooks and share links stay exempt.
	// NOTE: r.Use must come before any group/route registration, otherwise
	// gin silently drops the middleware for the earlier routes.
	var jwtKey []byte
	if authStore != nil {
		jwtKey = authStore.SigningKey()
	}
	entryGuard := secentry.NewGuard(settingsStore, authStore, jwtKey, log)
	r.Use(entryGuard.Middleware())

	// Panel security: strict domain access check and force HTTPS redirection.
	panelGuard := panelsec.NewGuard(settingsStore, log)
	r.Use(panelGuard.HTTPSMiddleware(), panelGuard.DomainMiddleware())

	v1 := r.Group("/api/v1")
	entryGuard.RegisterRoutes(v1)
	h := &handlers{reg: reg, log: log, sess: sessStore, auth: authStore, metrics: metricsStore, policy: policyStore, audit: auditStore, groups: groupStore, settings: settingsStore, certHub: certHub, panelCert: panelsec.NewPanelCertIssuer(settingsStore, certHub, log), alertMon: alertMon}

	// ---- Public routes (no auth) ----
	// Auth login.
	if authStore != nil {
		ah := auth.NewHandlers(authStore)
		v1.POST("/auth/login", h.loginWithAudit(ah.Login))
	}

	// Enroll (generates a one-time token; the install script uses it).
	v1.POST("/hosts/enroll", h.enroll)

	// Public share token info lookup for collaborative terminal guests
	v1.GET("/terminals/share/:token", h.getShareInfo)

	// ---- Authenticated routes ----
	authed := v1.Group("", auth.Middleware(authStore))

	// Role gates. A viewer is read-only by design (AGENTS.md 3.15/B.3), so any
	// route that changes a host — shell, file write, exec, docker, scan — needs
	// at least operator, and host lifecycle stays with admin.
	hostWrite := authed.Group("", auth.RequireRole(auth.RoleAdmin, auth.RoleOperator))
	adminOnly := authed.Group("", auth.RequireRole(auth.RoleAdmin))

	// Hosts.
	authed.GET("/hosts", h.listHosts)
	authed.GET("/hosts/:id", h.getHost)
	adminOnly.DELETE("/hosts/:id", h.deleteHost)
	adminOnly.PUT("/hosts/:id/group", h.setHostGroup)
	adminOnly.PUT("/hosts/:id/tags", h.setHostTags)
	adminOnly.PUT("/hosts/:id/billing", h.setHostBilling)

	// Terminal. An interactive shell is full write access to the host.
	hostWrite.POST("/hosts/:id/terminals", h.openTerminal)
	hostWrite.POST("/hosts/:id/terminals/:sid/share", h.shareTerminal)

	// Files: reads are open to every role, writes are not.
	authed.GET("/hosts/:id/files", h.fileList)
	authed.GET("/hosts/:id/files/stat", h.fileStat)
	authed.GET("/hosts/:id/files/download", h.fileDownload)
	// File operations on hosts are audited via the mutation middleware.
	fileWrite := hostWrite.Group("", h.auditMutation())
	fileWrite.POST("/hosts/:id/files/mkdir", h.fileMkdir)
	fileWrite.POST("/hosts/:id/files/move", h.fileMove)
	fileWrite.POST("/hosts/:id/files/copy", h.fileCopy)
	fileWrite.DELETE("/hosts/:id/files", h.fileRemove)
	fileWrite.POST("/hosts/:id/files/upload", h.fileUpload)

	// Exec (push command).
	hostWrite.POST("/hosts/:id/exec", h.execCommand)
	hostWrite.POST("/hosts/batch-exec", h.batchExec)

	// Metrics.
	authed.GET("/hosts/:id/metrics", h.getMetrics)
	authed.GET("/hosts/:id/metrics/history", h.getMetricsHistory)

	// SysInfo.
	authed.GET("/hosts/:id/sysinfo/:kind", h.getSysInfo)
	hostWrite.POST("/hosts/:id/processes/:pid/kill", h.killProcess)

	// Docker: inspection is read-only, container/image operations are not.
	authed.GET("/hosts/:id/docker/ps", h.dockerPs)
	authed.GET("/hosts/:id/docker/images", h.dockerImages)
	authed.GET("/hosts/:id/docker/all", h.dockerAll)
	authed.GET("/hosts/:id/docker/mirrors", h.dockerGetMirrors)
	hostWrite.PUT("/hosts/:id/docker/mirrors", h.dockerSetMirrors)
	hostWrite.POST("/hosts/:id/docker/:op", h.dockerOp)

	// App Store & templates. Installing an app or Docker runs commands on the
	// host, so those routes require a role that may change host state and are
	// recorded in the audit trail.
	apps.NewHandlers(reg).Register(authed,
		authed.Group("", auth.RequireRole(auth.RoleAdmin, auth.RoleOperator), h.auditMutation()))

	// Git-linked applications (Dokploy-style lifecycle: link repo, auto
	// deploy on push, rollback, build log). Mutations are audited; the
	// webhook endpoint is public with its own per-app token.
	if appStore != nil {
		appWrite := authed.Group("", auth.RequireRole(auth.RoleAdmin, auth.RoleOperator), h.auditMutation())
		appHandlers := apps.NewAppHandlers(reg, appStore, appEngine)
		if gitProviderStore != nil {
			appHandlers.SetGitProvider(gitProviderStore)
		}
		if aiAssistant != nil {
			appHandlers.SetAI(func(ctx context.Context, command, stdout, stderr string, exitCode int32) (any, error) {
				return aiAssistant.AnalyzeExecResult(ctx, command, stdout, stderr, exitCode)
			})
		}
		appHandlers.RegisterAppRoutes(
			authed.Group(""),
			appWrite,
			v1)
		// Reverse-proxy domain binding (cert + nginx vhost distribution),
		// available when both the app store and the cert hub are wired.
		if certHub != nil {
			ph := apps.NewProxyHandler(reg, appStore, certHub)
			ph.SetNetworkStore(networkStore)
			ph.Register(appWrite)
		}
	}

	// SSL certificate hub (ACME with compatible DNS-01/HTTP-01 challenge
	// solvers, plus manual certificate import). Certificate material is
	// sensitive, so mutations (config, issue, import, renew, delete) stay admin-only
	// and audited; reads are open to every authenticated role.
	if certHub != nil {
		cert.NewHandlers(certHub).Register(
			authed.Group(""),
			adminOnly.Group("", h.auditMutation()),
		)
	}

	// Host & container volume snapshots / database hot-backup (阶段 3).
	// Mutations execute commands on the managed host, so they require
	// hostWrite and are audited.
	if snapshotStore != nil {
		snapshots.NewHandlers(reg, snapshotStore, snapshotEngine).Register(
			authed.Group(""),
			hostWrite.Group("", h.auditMutation()),
		)
	}

	// Git provider integration (GitHub account authorization & repo selector).
	if gitProviderStore != nil {
		gitprovider.NewHandlers(gitProviderStore).Register(
			authed.Group(""),
			hostWrite.Group("", h.auditMutation()),
		)
	}

	// Security scanning.
	if scanStore != nil {
		scan.NewHandlers(reg, scanStore, log).Register(authed, hostWrite.Group("", h.auditMutation()))
	}

	// Sessions (audit / recordings). Removing a recording destroys audit
	// evidence, so only an admin may do it.
	authed.GET("/sessions", h.listSessions)
	authed.GET("/sessions/:id", h.getSession)
	authed.GET("/sessions/:id/recording", h.getRecording)
	adminOnly.DELETE("/sessions/:id", h.deleteSession)

	// Health.
	authed.GET("/system/health", h.health)

	// Version info: server build + agent upgrade source of truth.
	authed.GET("/version", h.versionInfo)
	authed.GET("/system/check-update", h.checkUpdate)

	// Panel certificate status & inspection.
	adminOnly.GET("/system/panel-cert", h.getPanelCertStatus)
	// One-click panel certificate issuance (domain or public IP), async jobs.
	adminOnly.POST("/system/panel-cert/issue", h.issuePanelCert)
	adminOnly.GET("/system/panel-cert/issue/:job_id", h.getPanelCertJob)
	adminOnly.POST("/system/panel-cert/issue/:job_id/confirm", h.confirmPanelCertIssue)
	adminOnly.DELETE("/system/panel-cert/issue/:job_id", h.cancelPanelCertIssue)
	adminOnly.GET("/system/panel-cert/public-ip", h.getPanelPublicIP)

	// Agent upgrade.
	adminOnly.POST("/hosts/:id/upgrade", h.upgradeAgent)

	// Control-server binary hot self-upgrade (upload file/base64 binary, online upgrade, restart).
	adminOnly.POST("/system/upgrade", h.systemUpgrade)
	adminOnly.POST("/system/online-upgrade", h.onlineUpgrade)
	adminOnly.POST("/system/restart", h.systemRestart)
	adminOnly.POST("/system/upgrade-agents", h.upgradeAgentsBatch)

	// User management (admin-only for mutating routes). Deleting an account
	// also drops its terminal preferences, so a recreated account starts fresh.
	if authStore != nil {
		uh := auth.NewHandlers(authStore)
		if settingsStore != nil {
			uh.OnUserDeleted(settingsStore.DeleteUser)
		}
		uh.Register(authed.Group("", h.auditMutation()))
	}

	// Alerts (rules + events + webhook). Mutations are audited.
	if alertStore != nil {
		alert.NewHandlers(alertStore).Register(authed.Group("", h.auditMutation()))
	}

	// Credential vault (admin-only).
	if vaultStore != nil {
		vault.NewHandlers(vaultStore).Register(authed.Group("", h.auditMutation()))
	}

	// AI diagnostics. Only config changes are audited (the middleware skips
	// the diagnostic POSTs, which have no mutation rules).
	if aiAssistant != nil {
		ai.NewHandlers(aiAssistant).Register(authed.Group("", h.auditMutation()))
	}

	// High-risk command control (policy + audit).
	if policyStore != nil {
		policy.NewHandlers(policyStore).Register(
			authed.Group("", h.auditMutation()),
			adminOnly.Group("", h.auditMutation()),
		)
	}

	// Unified operational audit trail (admin-only).
	if auditStore != nil {
		audit.NewHandlers(auditStore).Register(authed.Group("", auth.RequireRole(auth.RoleAdmin)))
	}

	// Saved-command library (used by 推送命令 and the settings page).
	if commandStore != nil {
		commands.NewHandlers(commandStore).Register(authed.Group("", h.auditMutation()))
	}

	// Host groups + per-group user authorization (mutations are admin-only).
	if groupStore != nil {
		groups.NewHandlers(groupStore, reg).Register(
			authed.Group(""),
			authed.Group("", auth.RequireRole(auth.RoleAdmin), h.auditMutation()),
		)
	}

	// Unified settings (Phase 2): user scope under /me, system scope admin-only.
	// Writes go through the audited groups so setting changes are logged.
	if settingsStore != nil {
		sh := settings.NewHandlers(settingsStore)
		sh.RegisterUser(authed, authed.Group("", h.auditMutation()))
		sh.RegisterSystem(adminOnly, adminOnly.Group("", h.auditMutation()))
	}

	// Control-plane backup / restore. An archive contains every credential,
	// recording and audit entry the server holds, and a restore rewrites all of
	// them, so these routes are admin-only and always audited.
	if backupStore != nil {
		backup.NewHandlers(backupStore).Register(
			authed.Group("", auth.RequireRole(auth.RoleAdmin), h.auditMutation()),
		)
	}

	// Overlay networking (Tailscale / Headscale).
	if networkStore != nil {
		network.NewHandlers(networkStore, reg, auditStore).Register(
			authed.Group(""),
			hostWrite.Group("", h.auditMutation()),
		)
	}

	// Reverse TCP tunnels (§3.9).
	h.registerTunnelRoutes(authed)

	// WebSocket endpoint (token via query param, since browsers can't set
	// Authorization headers on WebSocket upgrades easily; also supports share_token or code).
	r.GET("/api/v1/ws/terminal/:sid", func(c *gin.Context) {
		shareToken := c.Query("share_token")
		if shareToken == "" {
			shareToken = c.Query("code")
		}

		if shareToken != "" {
			if _, ok := ws.LookupShare(shareToken); !ok {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired share token"})
				return
			}
		} else if authStore != nil {
			tok := c.Query("token")
			if _, err := authStore.Validate(tok); err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid ws token"})
				return
			}
		}
		c.Request.SetPathValue("sid", c.Param("sid"))
		var onEnd func(string)
		if sessStore != nil {
			onEnd = func(sid string) { sessStore.End(sid) }
		}
		ws.TerminalHandler(reg, log, onEnd).ServeHTTP(c.Writer, c.Request)
	})

	// Web console (go:embed, see server/web). Mounted last: unknown /api/*
	// paths keep a JSON 404, everything else falls back to index.html
	// (Vue Router history mode). When the secure entry is enabled, the
	// guard middleware above already 404s unauthenticated requests here —
	// no reverse proxy needed to hide the pages.
	web.NewHandler().RegisterRoutes(r)

	return r
}

type handlers struct {
	reg       *rpc.Registry
	log       *slog.Logger
	sess      *session.Store
	auth      *auth.Store
	metrics   *metrics.Store
	policy    *policy.Store
	audit     *audit.Store
	groups    *groups.Store
	settings  *settings.Store
	certHub   *cert.Hub
	panelCert *panelsec.PanelCertIssuer
	alertMon  *alert.Monitor
}

// canSeeHost reports whether the caller is allowed to reach the given host,
// based on the host's group and the caller's group grants. Users without any
// grant (and admins) are unrestricted, so grouping stays optional.
func (h *handlers) canSeeHost(c *gin.Context, a *rpc.Agent) bool {
	if h.groups == nil || a == nil {
		return true
	}
	return h.groups.CanAccessGroup(auth.Username(c), auth.RoleOf(c) == auth.RoleAdmin, a.Group)
}

// recordAudit appends a unified audit entry, auto-filling the acting user and
// request metadata. Safe to call when auditing is disabled (nil store).
func (h *handlers) recordAudit(c *gin.Context, action, targetType, targetID, detail, risk, result string) {
	if h.audit == nil {
		return
	}
	ip, ua := audit.RequestInfo(c.Request)
	h.audit.Record(audit.Entry{
		Username:   auth.Username(c),
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Detail:     detail,
		IP:         ip,
		UserAgent:  ua,
		RiskLevel:  risk,
		Result:     result,
	})
}

// loginWithAudit wraps the login handler so successful and failed console
// logins are recorded in the audit trail (login runs before auth middleware,
// so the username is taken from the request body).
func (h *handlers) loginWithAudit(login gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		var username string
		if raw, err := io.ReadAll(c.Request.Body); err == nil {
			var body struct {
				Username string `json:"username"`
			}
			_ = json.Unmarshal(raw, &body)
			username = body.Username
			// Restore the body for the real handler.
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		}
		login(c)
		if h.audit == nil {
			return
		}
		result := audit.ResultSuccess
		if c.Writer.Status() != http.StatusOK {
			result = audit.ResultFailed
		}
		ip, ua := audit.RequestInfo(c.Request)
		h.audit.Record(audit.Entry{
			Username:   username,
			Action:     "login",
			TargetType: "system",
			IP:         ip,
			UserAgent:  ua,
			RiskLevel:  audit.RiskMedium,
			Result:     result,
		})
	}
}

func (h *handlers) enroll(c *gin.Context) {
	token := h.reg.IssueEnrollToken()
	serverURL := ""
	if h.settings != nil {
		serverURL = h.settings.PanelSecurity().PublicURL
	}
	if serverURL == "" {
		scheme := "http"
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		serverURL = fmt.Sprintf("%s://%s", scheme, c.Request.Host)
	}
	serverURL = strings.TrimRight(serverURL, "/")
	c.JSON(http.StatusOK, gin.H{
		"enroll_token": token,
		"expires_in":   86400,
		"install":      fmt.Sprintf("curl -kfsSL '%s/install?token=%s' | sudo bash", serverURL, token),
		"install_win":  fmt.Sprintf("irm '%s/install?os_type=windows^&token=%s' | iex", serverURL, token),
	})
}

func (h *handlers) getPanelCertStatus(c *gin.Context) {
	if h.settings == nil {
		c.JSON(http.StatusOK, gin.H{"ssl_enabled": false})
		return
	}
	sec := h.settings.PanelSecurity()
	res := gin.H{
		"ssl_enabled":  sec.SSLEnabled,
		"ssl_mode":     sec.SSLMode,
		"ssl_cert_id":  sec.SSLCertID,
		"panel_domain": sec.PanelDomain,
		// 绑定域名即自动启用严格域名限制（仅允许该域名与回环地址访问）。
		"strict_domain": sec.PanelDomain != "",
		"force_https":   sec.ForceHTTPS,
		"public_url":    sec.PublicURL,
		"active":        false,
	}
	if h.certHub != nil || sec.SSLMode == "custom" {
		cp := panelsec.NewCertProvider(h.settings, h.certHub, nil, h.log)
		if summary, err := cp.InspectActiveCert(); err == nil && summary != nil {
			res["active"] = summary.Valid
			if sec.SSLMode == "custom" {
				res["source"] = "自定义上传"
			} else {
				res["source"] = "证书中心"
			}
			res["subject"] = summary.Subject
			res["issuer"] = summary.Issuer
			res["dns_names"] = summary.Domains
			res["not_after"] = summary.NotAfter.Format(time.RFC3339)
			res["days_left"] = summary.DaysRemaining
		} else if err != nil {
			res["error"] = err.Error()
		}
	}
	c.JSON(http.StatusOK, res)
}

// ---- Panel one-click certificate issuance ----

// issuePanelCert starts an async panel certificate issuance job.
// Body: {"mode": "domain"|"ip", "ip": "1.2.3.4" (optional for ip mode)}.
// mode=domain issues for the bound panel domain (HTTP-01 preferred, DNS-01
// via dns-mng as fallback); mode=ip issues a free IP certificate for a
// public IP (HTTP-01 on port 80).
// Returns 202 with the job; poll getPanelCertJob for progress.
func (h *handlers) issuePanelCert(c *gin.Context) {
	var req struct {
		Mode      string `json:"mode"`
		IP        string `json:"ip"`
		Challenge string `json:"challenge"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.panelCert == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "证书签发器不可用"})
		return
	}
	job, err := h.panelCert.StartIssue(strings.TrimSpace(req.Mode), strings.TrimSpace(req.IP), strings.TrimSpace(req.Challenge))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.recordAudit(c, "panel_cert_issue", "system", "panel-cert",
		fmt.Sprintf("启动面板证书签发任务(%s %s): %s", req.Mode, job.Target, job.ID),
		audit.RiskHigh, audit.ResultSuccess)
	c.JSON(http.StatusAccepted, gin.H{"data": job, "message": "签发任务已启动，可通过任务 ID 查询进度"})
}

func (h *handlers) getPanelCertJob(c *gin.Context) {
	if h.panelCert == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "证书签发器不可用"})
		return
	}
	job, ok := h.panelCert.GetJob(c.Param("job_id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "签发任务不存在或已过期"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": job})
}

// confirmPanelCertIssue starts CA validation for a manual DNS-01 panel job.
// The administrator must have provisioned the TXT records first; the job
// stays async, poll getPanelCertJob for progress.
func (h *handlers) confirmPanelCertIssue(c *gin.Context) {
	if h.panelCert == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "证书签发器不可用"})
		return
	}
	job, err := h.panelCert.ConfirmIssue(c.Param("job_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.recordAudit(c, "panel_cert_issue_confirm", "system", "panel-cert",
		fmt.Sprintf("确认面板证书手动 DNS-01 验证(%s): %s", job.Target, job.ID),
		audit.RiskHigh, audit.ResultSuccess)
	c.JSON(http.StatusAccepted, gin.H{"data": job, "message": "已通知 CA 开始验证，可通过任务 ID 查询进度"})
}

// cancelPanelCertIssue discards a manual DNS-01 panel job that is no longer
// needed (e.g. the administrator gave up adding the TXT records). The
// underlying ACME pending order is cancelled as well.
func (h *handlers) cancelPanelCertIssue(c *gin.Context) {
	if h.panelCert == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "证书签发器不可用"})
		return
	}
	jobID := c.Param("job_id")
	if err := h.panelCert.CancelIssue(jobID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.recordAudit(c, "panel_cert_issue_cancel", "system", "panel-cert",
		fmt.Sprintf("取消面板证书手动 DNS-01 签发任务: %s", jobID),
		audit.RiskHigh, audit.ResultSuccess)
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "已取消签发任务"})
}

// getPanelPublicIP detects the server's public egress IP (for IP certs).
func (h *handlers) getPanelPublicIP(c *gin.Context) {
	ip, err := cert.DetectPublicIP()
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ip": ip}})
}

// ---- Hosts -----------------------------------------------------------

func (h *handlers) listHosts(c *gin.Context) {
	agents := h.reg.ListAgents()
	out := make([]HostDTO, 0, len(agents))
	for _, a := range agents {
		if !h.canSeeHost(c, a) {
			continue
		}
		out = append(out, h.toDTO(a))
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "total": len(out)})
}

func (h *handlers) getHost(c *gin.Context) {
	a := h.reg.GetAgent(c.Param("id"))
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}
	if !h.canSeeHost(c, a) {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权访问该主机所属分组"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.toDTO(a)})
}

func (h *handlers) deleteHost(c *gin.Context) {
	id := c.Param("id")
	uninstallAgent := c.DefaultQuery("uninstall_agent", "false") == "true"

	uninstalled := false
	uninstallNote := ""
	if uninstallAgent {
		hub := h.reg.Hub(id)
		if hub == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "agent offline: cannot remote-uninstall; unbind without uninstall, or bring the host online first"})
			return
		}
		// Ask the agent to uninstall itself, then wait for it to go
		// offline (it exits after cleanup). Best-effort: old agents
		// ignore the message and stay online.
		hub.Send(&agentpb.ServerMessage{
			Payload: &agentpb.ServerMessage_Uninstall{
				Uninstall: &agentpb.UninstallRequest{RemoveData: true},
			},
		})
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if h.reg.Hub(id) == nil {
				uninstalled = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !uninstalled {
			uninstallNote = "agent 未确认卸载（可能版本过旧不支持远程卸载），请手动处理被管机"
			h.log.Warn("host unbind: agent did not go offline after uninstall request", "host_id", id)
		}
	}

	if err := h.reg.DeleteAgent(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	detail := "解绑并移除主机"
	if uninstallAgent {
		if uninstalled {
			detail = "解绑主机并远程卸载 Agent"
		} else {
			detail = "解绑主机（Agent 远程卸载未确认）"
		}
	}
	h.recordAudit(c, "host_unbind", "host", id, detail, audit.RiskHigh, audit.ResultSuccess)
	resp := gin.H{"ok": true, "uninstalled": uninstalled}
	if uninstallNote != "" {
		resp["warning"] = uninstallNote
	}
	c.JSON(http.StatusOK, resp)
}

func (h *handlers) setHostGroup(c *gin.Context) {
	var body struct {
		Group string `json:"group"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.reg.SetAgentGroup(c.Param("id"), body.Group); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	h.recordAudit(c, "host_group", "host", c.Param("id"), "设置分组: "+body.Group, audit.RiskLow, audit.ResultSuccess)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *handlers) setHostTags(c *gin.Context) {
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.reg.SetAgentTags(c.Param("id"), body.Tags); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	h.recordAudit(c, "host_tags", "host", c.Param("id"), "更新标签", audit.RiskLow, audit.ResultSuccess)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *handlers) setHostBilling(c *gin.Context) {
	var body rpc.HostBillingConfig
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.reg.SetAgentBilling(c.Param("id"), body); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	h.recordAudit(c, "host_billing", "host", c.Param("id"), "更新主机财务与流量配置", audit.RiskLow, audit.ResultSuccess)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- Terminal --------------------------------------------------------

func (h *handlers) openTerminal(c *gin.Context) {
	agentID := c.Param("id")
	a := h.reg.GetAgent(agentID)
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}
	if h.reg.Hub(agentID) == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}
	var body struct {
		Shell string `json:"shell"`
	}
	_ = c.ShouldBindJSON(&body)

	// An unspecified shell falls back to the caller's saved default, so the
	// preference applies even when the request comes from a client that does
	// not read preferences (share links, older UI, API scripts).
	if body.Shell == "" && h.settings != nil {
		body.Shell = h.settings.Terminal(auth.Username(c)).DefaultShell
	}

	sid := randomToken(12)
	ws.RegisterSession(sid, agentID, body.Shell)
	// Record the session start for audit.
	if h.sess != nil {
		operator := auth.Username(c)
		h.sess.Start(sid, agentID, a.Hostname, operator)
	}
	shell := body.Shell
	if shell == "" {
		shell = "default"
	}
	h.recordAudit(c, "terminal_open", "host", agentID,
		fmt.Sprintf("打开终端会话 %s (shell=%s, 主机=%s)", sid, shell, a.Hostname),
		audit.RiskMedium, audit.ResultSuccess)
	// Build the WS URL with the caller's token so the browser can authenticate.
	wsURL := "/api/v1/ws/terminal/" + sid
	if tok := c.Query("ws_token"); tok != "" {
		wsURL += "?token=" + tok
	}
	c.JSON(http.StatusOK, gin.H{
		"session_id": sid,
		"ws_url":     wsURL,
	})
}

func (h *handlers) shareTerminal(c *gin.Context) {
	agentID := c.Param("id")
	sid := c.Param("sid")
	sessInfo, ok := ws.LookupSessionInfo(sid)
	if !ok || sessInfo.AgentID != agentID {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	var body struct {
		ExpireMinutes int    `json:"expire_minutes"`
		Mode          string `json:"mode"` // "view" | "control"
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		body.ExpireMinutes = 15
		body.Mode = "view"
	}
	if body.ExpireMinutes <= 0 {
		body.ExpireMinutes = 15
	}
	if body.Mode != "control" {
		body.Mode = "view"
	}

	share, err := ws.CreateShare(sid, agentID, body.Mode, time.Duration(body.ExpireMinutes)*time.Minute)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.recordAudit(c, "terminal_share", "session", sid,
		fmt.Sprintf("分享终端会话 (模式=%s, 有效期=%d分钟, 主机=%s)", body.Mode, body.ExpireMinutes, agentID),
		audit.RiskMedium, audit.ResultSuccess)

	c.JSON(http.StatusOK, gin.H{
		"share_token": share.Token,
		"share_code":  share.Code,
		"session_id":  share.SessionID,
		"mode":        share.Mode,
		"expires_at":  share.ExpiresAt.Format(time.RFC3339),
	})
}

func (h *handlers) getShareInfo(c *gin.Context) {
	token := c.Param("token")
	share, ok := ws.LookupShare(token)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "share link expired or invalid"})
		return
	}
	agent := h.reg.GetAgent(share.AgentID)
	hostname := share.AgentID
	if agent != nil {
		hostname = agent.Hostname
	}
	c.JSON(http.StatusOK, gin.H{
		"session_id": share.SessionID,
		"host_id":    share.AgentID,
		"hostname":   hostname,
		"mode":       share.Mode,
		"expires_at": share.ExpiresAt.Format(time.RFC3339),
	})
}

// ---- Exec (push command) --------------------------------------------

func (h *handlers) execCommand(c *gin.Context) {
	var body struct {
		Command  string `json:"command"`
		Shell    string `json:"shell"`
		Timeout  int32  `json:"timeout_sec"`
		IsScript bool   `json:"is_script"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// High-risk command policy check.
	risk := audit.RiskLow
	if h.policy != nil {
		res := h.policy.Check(body.Command)
		if res.RiskLevel == policy.RiskHigh {
			risk = audit.RiskHigh
		}
		username := auth.Username(c)
		hostID := c.Param("id")
		if !res.Allowed {
			h.policy.RecordAudit(policy.AuditEntry{
				Username: username, HostID: hostID, Command: body.Command, Shell: body.Shell,
				RiskLevel: res.RiskLevel, Result: "blocked", Reason: res.Reason,
			})
			h.recordAudit(c, "exec", "host", hostID,
				"命令被策略拦截: "+body.Command+" ("+res.Reason+")",
				audit.RiskHigh, audit.ResultBlocked)
			c.JSON(http.StatusForbidden, gin.H{"error": res.Reason, "risk_level": res.RiskLevel, "matched_pattern": res.MatchedPattern})
			return
		}
		if res.NeedsConfirm && c.GetHeader("X-Confirm-Risk") != "true" {
			h.policy.RecordAudit(policy.AuditEntry{
				Username: username, HostID: hostID, Command: body.Command, Shell: body.Shell,
				RiskLevel: res.RiskLevel, Result: "denied", Reason: "未确认高危命令",
			})
			h.recordAudit(c, "exec", "host", hostID,
				"高危命令等待二次确认: "+body.Command,
				audit.RiskHigh, audit.ResultBlocked)
			c.JSON(http.StatusConflict, gin.H{
				"error":           res.Reason,
				"risk_level":      res.RiskLevel,
				"matched_pattern": res.MatchedPattern,
				"needs_confirm":   true,
			})
			return
		}
		// Allowed (confirmed or low-risk): record audit.
		resultLabel := "allowed"
		if res.NeedsConfirm {
			resultLabel = "confirmed"
		}
		h.policy.RecordAudit(policy.AuditEntry{
			Username: username, HostID: hostID, Command: body.Command, Shell: body.Shell,
			RiskLevel: res.RiskLevel, Result: resultLabel, Reason: res.Reason,
		})
	}

	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	execID := randomToken(8)
	resultCh := make(chan *agentpb.ExecResult, 1)
	hub.SetRespHandler(execID, func(msg *agentpb.AgentMessage) {
		if msg == nil {
			resultCh <- nil
			return
		}
		resultCh <- msg.GetExecResult()
	})

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:     execID,
				Shell:      body.Shell,
				Command:    body.Command,
				IsScript:   body.IsScript,
				TimeoutSec: body.Timeout,
			},
		},
	})

	defer hub.SetRespHandler(execID, nil)
	// Wait long enough for the requested command. A one-liner defaults to 60s,
	// but an install script (Docker, etc.) can legitimately run for minutes, so
	// honor the caller's timeout_sec plus round-trip headroom, capped so a stuck
	// command can't pin a goroutine indefinitely.
	waitFor := 120 * time.Second
	if body.Timeout > 0 {
		waitFor = time.Duration(body.Timeout+30) * time.Second
	}
	if waitFor > 900*time.Second {
		waitFor = 900 * time.Second
	}
	select {
	case res := <-resultCh:
		if res == nil {
			h.recordAudit(c, "exec", "host", c.Param("id"), "执行命令失败(agent断开): "+body.Command, risk, audit.ResultFailed)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		result := audit.ResultSuccess
		if res.GetExitCode() != 0 || res.GetError() != "" {
			result = audit.ResultFailed
		}
		h.recordAudit(c, "exec", "host", c.Param("id"),
			fmt.Sprintf("执行命令 (shell=%s, 退出码=%d): %s", body.Shell, res.GetExitCode(), body.Command),
			risk, result)
		c.JSON(http.StatusOK, gin.H{
			"exec_id":     res.GetExecId(),
			"exit_code":   res.GetExitCode(),
			"stdout":      string(res.GetStdout()),
			"stderr":      string(res.GetStderr()),
			"duration_ms": res.GetDurationMs(),
			"error":       res.GetError(),
		})
	case <-time.After(waitFor):
		h.recordAudit(c, "exec", "host", c.Param("id"), "执行命令超时: "+body.Command, risk, audit.ResultFailed)
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "exec timeout"})
	}
}

func (h *handlers) batchExec(c *gin.Context) {
	var body struct {
		HostIDs []string `json:"host_ids"`
		Command string   `json:"command"`
		Shell   string   `json:"shell"`
		Timeout int32    `json:"timeout_sec"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// High-risk command policy check (batch).
	batchRisk := audit.RiskLow
	if h.policy != nil {
		res := h.policy.Check(body.Command)
		if res.RiskLevel == policy.RiskHigh {
			batchRisk = audit.RiskHigh
		}
		username := auth.Username(c)
		if !res.Allowed {
			h.policy.RecordAudit(policy.AuditEntry{
				Username: username, HostIDs: body.HostIDs, Command: body.Command, Shell: body.Shell,
				RiskLevel: res.RiskLevel, Result: "blocked", Reason: res.Reason,
			})
			h.recordAudit(c, "batch_exec", "host", strings.Join(body.HostIDs, ","),
				fmt.Sprintf("批量命令被策略拦截 (%d 台主机): %s", len(body.HostIDs), body.Command),
				audit.RiskHigh, audit.ResultBlocked)
			c.JSON(http.StatusForbidden, gin.H{"error": res.Reason, "risk_level": res.RiskLevel, "matched_pattern": res.MatchedPattern})
			return
		}
		if res.NeedsConfirm && c.GetHeader("X-Confirm-Risk") != "true" {
			h.policy.RecordAudit(policy.AuditEntry{
				Username: username, HostIDs: body.HostIDs, Command: body.Command, Shell: body.Shell,
				RiskLevel: res.RiskLevel, Result: "denied", Reason: "未确认高危命令",
			})
			h.recordAudit(c, "batch_exec", "host", strings.Join(body.HostIDs, ","),
				fmt.Sprintf("批量高危命令等待二次确认 (%d 台主机): %s", len(body.HostIDs), body.Command),
				audit.RiskHigh, audit.ResultBlocked)
			c.JSON(http.StatusConflict, gin.H{
				"error":           res.Reason,
				"risk_level":      res.RiskLevel,
				"matched_pattern": res.MatchedPattern,
				"needs_confirm":   true,
			})
			return
		}
		resultLabel := "allowed"
		if res.NeedsConfirm {
			resultLabel = "confirmed"
		}
		h.policy.RecordAudit(policy.AuditEntry{
			Username: username, HostIDs: body.HostIDs, Command: body.Command, Shell: body.Shell,
			RiskLevel: res.RiskLevel, Result: resultLabel, Reason: res.Reason,
		})
	}

	type result struct {
		HostID   string `json:"host_id"`
		ExitCode int32  `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		Error    string `json:"error"`
	}
	results := make([]result, 0, len(body.HostIDs))
	done := make(chan result, len(body.HostIDs))

	for _, hid := range body.HostIDs {
		hub := h.reg.Hub(hid)
		if hub == nil {
			done <- result{HostID: hid, Error: "agent offline"}
			continue
		}
		execID := randomToken(8)
		hidCopy := hid
		hub.SetRespHandler(execID, func(msg *agentpb.AgentMessage) {
			hub.SetRespHandler(execID, nil)
			if msg == nil {
				done <- result{HostID: hidCopy, Error: "agent disconnected"}
				return
			}
			r := msg.GetExecResult()
			done <- result{
				HostID:   hidCopy,
				ExitCode: r.GetExitCode(),
				Stdout:   string(r.GetStdout()),
				Stderr:   string(r.GetStderr()),
				Error:    r.GetError(),
			}
		})
		hub.Send(&agentpb.ServerMessage{
			Payload: &agentpb.ServerMessage_Exec{
				Exec: &agentpb.ExecRequest{
					ExecId:     execID,
					Shell:      body.Shell,
					Command:    body.Command,
					TimeoutSec: body.Timeout,
				},
			},
		})
	}

	for i := 0; i < len(body.HostIDs); i++ {
		select {
		case r := <-done:
			results = append(results, r)
		case <-time.After(120 * time.Second):
			results = append(results, result{Error: "timeout"})
		}
	}
	failed := 0
	for _, r := range results {
		if r.Error != "" || r.ExitCode != 0 {
			failed++
		}
	}
	resultLabel := audit.ResultSuccess
	if failed > 0 {
		resultLabel = audit.ResultFailed
	}
	h.recordAudit(c, "batch_exec", "host", strings.Join(body.HostIDs, ","),
		fmt.Sprintf("批量推送命令到 %d 台主机 (%d 台异常): %s", len(body.HostIDs), failed, body.Command),
		batchRisk, resultLabel)
	c.JSON(http.StatusOK, gin.H{"results": results})
}

// ---- Metrics ---------------------------------------------------------

func (h *handlers) getMetrics(c *gin.Context) {
	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	// Always request a fresh one-shot sample so the agent can compute
	// per-second rates by diffing cumulative counters across calls.
	// Fall back to the cached sample if the agent doesn't respond in time.
	lm := hub.LastMetrics()

	resultCh := make(chan *agentpb.MetricsSample, 1)
	hub.SetRespHandler("metrics-live", func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler("metrics-live", nil)
		if msg != nil {
			resultCh <- msg.GetMetrics()
		}
	})
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_MetricsQ{
			MetricsQ: &agentpb.MetricsQuery{Live: false},
		},
	})
	select {
	case m := <-resultCh:
		if m == nil {
			if lm != nil {
				c.JSON(http.StatusOK, metricsToJSON(lm))
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no metrics"})
			return
		}
		if h.metrics != nil {
			h.metrics.AddSample(c.Param("id"), m)
		}
		c.JSON(http.StatusOK, metricsToJSON(m))
	case <-time.After(3 * time.Second):
		hub.SetRespHandler("metrics-live", nil)
		if lm != nil {
			c.JSON(http.StatusOK, metricsToJSON(lm))
			return
		}
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "metrics timeout"})
	}
}

func metricsToJSON(m *agentpb.MetricsSample) gin.H {
	mounts := make([]gin.H, 0, len(m.GetMounts()))
	for _, mt := range m.GetMounts() {
		mounts = append(mounts, gin.H{
			"path":  mt.GetPath(),
			"total": mt.GetTotal(),
			"used":  mt.GetUsed(),
		})
	}
	return gin.H{
		"ts":              m.GetTs(),
		"cpu_usage":       m.GetCpuUsage(),
		"mem_usage":       m.GetMemUsage(),
		"mem_total":       m.GetMemTotal(),
		"mem_used":        m.GetMemUsed(),
		"net_rx":          m.GetNetRx(),
		"net_tx":          m.GetNetTx(),
		"disk_read":       m.GetDiskRead(),
		"disk_write":      m.GetDiskWrite(),
		"mounts":          mounts,
		"load1":           m.GetLoad1(),
		"load5":           m.GetLoad5(),
		"load15":          m.GetLoad15(),
		"swap_total":      m.GetSwapTotal(),
		"swap_used":       m.GetSwapUsed(),
		"tcp_established": m.GetTcpEstablished(),
		"udp_count":       m.GetUdpCount(),
		"process_count":   m.GetProcessCount(),
		"cpu_model":       m.GetCpuModel(),
		"month_rx":        m.GetMonthRx(),
		"month_tx":        m.GetMonthTx(),
	}
}

func (h *handlers) getMetricsHistory(c *gin.Context) {
	if h.metrics == nil {
		c.JSON(http.StatusOK, gin.H{"points": []any{}})
		return
	}
	hostID := c.Param("id")
	fromStr := c.Query("from")
	toStr := c.Query("to")
	stepStr := c.Query("step")

	var from, to int64
	var step int
	if v, err := strconv.ParseInt(fromStr, 10, 64); err == nil {
		from = v
	}
	if v, err := strconv.ParseInt(toStr, 10, 64); err == nil {
		to = v
	}
	if v, err := strconv.Atoi(stepStr); err == nil {
		step = v
	}

	if to == 0 {
		to = time.Now().Unix()
	}
	if from == 0 {
		from = to - 3600 // default 1 hour
	}
	if step <= 0 {
		step = 15
	}

	points := h.metrics.QueryHistory(hostID, from, to, step)
	c.JSON(http.StatusOK, gin.H{
		"host_id": hostID,
		"from":    from,
		"to":      to,
		"step":    step,
		"points":  points,
	})
}

// ---- SysInfo ---------------------------------------------------------

func (h *handlers) getSysInfo(c *gin.Context) {
	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}
	kind := c.Param("kind")
	if kind == "kill" {
		// Killing is a mutation and has its own audited route; refuse to reach it
		// through the read-only sysinfo path.
		c.JSON(http.StatusBadRequest, gin.H{"error": "请使用 POST /hosts/:id/processes/:pid/kill"})
		return
	}
	h.sysInfoQuery(c, hub, &agentpb.SysInfoQuery{Kind: kind})
}

// killProcess terminates a single process on the target host. Admin/operator
// only, group-scoped, and audited as a high-risk action.
func (h *handlers) killProcess(c *gin.Context) {
	a := h.reg.GetAgent(c.Param("id"))
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}
	if !h.canSeeHost(c, a) {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权访问该主机所属分组"})
		return
	}
	pid, err := strconv.Atoi(c.Param("pid"))
	if err != nil || pid <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "非法 PID"})
		return
	}
	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	var body struct {
		Force bool   `json:"force"`
		Name  string `json:"name"`
	}
	_ = c.ShouldBindJSON(&body)

	signal := "SIGTERM"
	if body.Force {
		signal = "SIGKILL"
	}
	detail := fmt.Sprintf("结束进程 PID=%d (%s)", pid, signal)
	if body.Name != "" {
		detail = fmt.Sprintf("结束进程 %s PID=%d (%s)", body.Name, pid, signal)
	}

	status, payload := h.sysInfoCall(hub, &agentpb.SysInfoQuery{
		Kind:  "kill",
		Pid:   int32(pid),
		Force: body.Force,
	})
	result := audit.ResultSuccess
	if status != http.StatusOK {
		result = audit.ResultFailed
	}
	h.recordAudit(c, "process_kill", "host", c.Param("id"), detail, audit.RiskHigh, result)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"error": string(payload)})
		return
	}
	c.Data(http.StatusOK, "application/json", payload)
}

// sysInfoQuery issues a SysInfoQuery and writes the agent's raw JSON payload to
// the response.
func (h *handlers) sysInfoQuery(c *gin.Context, hub *rpc.Hub, q *agentpb.SysInfoQuery) {
	status, payload := h.sysInfoCall(hub, q)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"error": string(payload)})
		return
	}
	// Return the raw JSON payload from the agent.
	c.Data(http.StatusOK, "application/json", payload)
}

// sysInfoCall performs one request/response round trip over the agent hub. On
// success it returns (200, jsonPayload); on failure (status, errorText).
func (h *handlers) sysInfoCall(hub *rpc.Hub, q *agentpb.SysInfoQuery) (int, []byte) {
	ref := "sysinfo:" + q.GetKind()
	resultCh := make(chan []byte, 1)
	hub.SetRespHandler(ref, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(ref, nil)
		if msg == nil {
			resultCh <- nil
			return
		}
		si := msg.GetSysinfo()
		if si != nil {
			resultCh <- si.GetJsonPayload()
		} else {
			resultCh <- nil
		}
	})
	defer hub.SetRespHandler(ref, nil)

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_SysinfoQ{SysinfoQ: q},
	})

	select {
	case data := <-resultCh:
		if data == nil {
			return http.StatusServiceUnavailable, []byte("no response")
		}
		// The agent reports failures as {"error":"..."} through the same channel.
		var probe struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &probe) == nil && probe.Error != "" {
			return http.StatusInternalServerError, []byte(probe.Error)
		}
		return http.StatusOK, data
	case <-time.After(10 * time.Second):
		return http.StatusGatewayTimeout, []byte("sysinfo timeout")
	}
}

// ---- Docker ----------------------------------------------------------

func (h *handlers) dockerPs(c *gin.Context) {
	h.dockerCall(c, "ps", "", "", nil)
}

func (h *handlers) dockerImages(c *gin.Context) {
	h.dockerCall(c, "images", "", "", nil)
}

// dockerAll fetches containers + images in a single round trip to the agent
// (op "ps_images"), collapsing two HTTP calls into one for the dashboard.
func (h *handlers) dockerAll(c *gin.Context) {
	h.dockerCall(c, "ps_images", "", "", nil)
}

func (h *handlers) dockerOp(c *gin.Context) {
	op := c.Param("op")
	var body struct {
		Container string          `json:"container"`
		Image     string          `json:"image"`
		Args      json.RawMessage `json:"args_json"` // run op options (name/ports/volumes/env/...)
	}
	_ = c.ShouldBindJSON(&body)
	// Mutating docker operations are audited; read-only ones (ps/images) are not.
	switch op {
	case "start", "stop", "restart", "rm", "pull", "rmi", "prune", "remove_image", "run":
		target := body.Container
		if target == "" {
			target = body.Image
		}
		risk := audit.RiskLow
		if op == "rm" || op == "rmi" || op == "remove_image" || op == "prune" {
			risk = audit.RiskHigh
		}
		h.recordAudit(c, "docker_op", "host", c.Param("id"),
			fmt.Sprintf("Docker 操作 %s %s", op, target), risk, audit.ResultSuccess)
	}
	h.dockerCall(c, op, body.Container, body.Image, body.Args)
}

func (h *handlers) dockerCall(c *gin.Context, op, container, image string, argsJSON []byte) {
	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}
	opID := randomToken(8)
	resultCh := make(chan *agentpb.DockerEvent, 1)
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(opID, nil)
		if msg == nil {
			resultCh <- nil
			return
		}
		resultCh <- msg.GetDocker()
	})
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_DockerOp{
			DockerOp: &agentpb.DockerOp{
				OpId:      opID,
				Op:        op,
				Container: container,
				Image:     image,
				ArgsJson:  argsJSON,
			},
		},
	})
	defer hub.SetRespHandler(opID, nil)
	select {
	case ev := <-resultCh:
		if ev == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		if !ev.GetOk() {
			c.JSON(http.StatusInternalServerError, gin.H{"error": ev.GetError()})
			return
		}
		// Parse the payload JSON for a structured response. Docker's
		// --format {{json .}} emits NDJSON (one object per line), which is
		// not a single JSON document, so try line-by-line parsing as a
		// fallback before returning the raw text.
		payload := ev.GetPayloadJson()
		// ps/images are lists. `docker --format {{json .}}` prints one object per
		// line, so a single container is a valid standalone JSON object while two
		// or more are NDJSON. Parsing the document first would hand the browser
		// an object in the one-container case and render an empty table, so list
		// ops are always normalised to an array.
		if op == "ps" || op == "images" {
			rows := parseNDJSON(payload)
			if rows == nil {
				rows = []any{}
			}
			c.JSON(http.StatusOK, gin.H{"ok": true, "data": rows})
			return
		}
		var parsed any
		if json.Unmarshal(payload, &parsed) == nil {
			c.JSON(http.StatusOK, gin.H{"ok": true, "data": parsed})
		} else if rows := parseNDJSON(payload); rows != nil {
			c.JSON(http.StatusOK, gin.H{"ok": true, "data": rows})
		} else {
			c.JSON(http.StatusOK, gin.H{"ok": true, "raw": string(payload)})
		}
	case <-time.After(dockerOpTimeout(op)):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "docker timeout"})
	}
}

// dockerOpTimeout returns the HTTP-side wait budget for a docker op. Pulls can
// take minutes, and `docker run` pulls a missing image before creating the
// container, so both match the agent's longer budget instead of forcing a 504
// at 30s while the agent keeps working.
func dockerOpTimeout(op string) time.Duration {
	if op == "pull" || op == "run" {
		return 5 * time.Minute
	}
	return 30 * time.Second
}

// parseNDJSON parses newline-delimited JSON into a []any. Returns nil if any
// non-empty line fails to parse or there are no non-empty lines.
func parseNDJSON(raw []byte) []any {
	var rows []any
	seen := false
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		seen = true
		var v any
		if json.Unmarshal(line, &v) != nil {
			return nil
		}
		rows = append(rows, v)
	}
	if !seen {
		return nil
	}
	return rows
}

// dockerGetMirrors reads the Docker daemon registry-mirrors configuration from the agent.
func (h *handlers) dockerGetMirrors(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	cmd := `if [ -f /etc/docker/daemon.json ]; then cat /etc/docker/daemon.json; else echo "{}"; fi`
	shell := "sh"
	agent := h.reg.GetAgent(hostID)
	if agent != nil && strings.Contains(strings.ToLower(agent.OS), "windows") {
		cmd = `$p = "$env:ProgramData\Docker\config\daemon.json"; if (Test-Path $p) { Get-Content $p -Raw } else { "{}" }`
		shell = "powershell"
	}

	execID := randomToken(8)
	resultCh := make(chan *agentpb.ExecResult, 1)
	hub.SetRespHandler(execID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(execID, nil)
		if msg != nil {
			resultCh <- msg.GetExecResult()
		} else {
			resultCh <- nil
		}
	})

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:     execID,
				Shell:      shell,
				Command:    cmd,
				IsScript:   false,
				TimeoutSec: 15,
			},
		},
	})
	defer hub.SetRespHandler(execID, nil)

	select {
	case res := <-resultCh:
		if res == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		if res.GetExitCode() != 0 {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read daemon.json: " + string(res.GetStderr())})
			return
		}
		var daemonCfg struct {
			RegistryMirrors []string `json:"registry-mirrors"`
		}
		out := bytes.TrimSpace(res.GetStdout())
		if len(out) > 0 {
			_ = json.Unmarshal(out, &daemonCfg)
		}
		if daemonCfg.RegistryMirrors == nil {
			daemonCfg.RegistryMirrors = []string{}
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "mirrors": daemonCfg.RegistryMirrors})
	case <-time.After(20 * time.Second):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "timeout reading mirrors from host"})
	}
}

// dockerSetMirrors updates the Docker daemon registry-mirrors configuration on the agent and reloads Docker.
func (h *handlers) dockerSetMirrors(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	var body struct {
		Mirrors []string `json:"mirrors"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	cleanMirrors := make([]string, 0, len(body.Mirrors))
	for _, m := range body.Mirrors {
		m = strings.TrimSpace(m)
		if m != "" {
			cleanMirrors = append(cleanMirrors, m)
		}
	}

	mirrorsJSON, _ := json.Marshal(cleanMirrors)
	agent := h.reg.GetAgent(hostID)
	isWindows := agent != nil && strings.Contains(strings.ToLower(agent.OS), "windows")

	var cmd, shell string
	if isWindows {
		shell = "powershell"
		cmd = fmt.Sprintf(`$cfgPath = "$env:ProgramData\Docker\config\daemon.json"
$dir = Split-Path $cfgPath
if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
$cfg = @{}
if (Test-Path $cfgPath) {
    try { $cfg = Get-Content $cfgPath -Raw | ConvertFrom-Json -AsHashtable } catch { $cfg = @{} }
}
$m = '%s' | ConvertFrom-Json
$cfg["registry-mirrors"] = $m
$cfg | ConvertTo-Json -Depth 10 | Set-Content $cfgPath -Encoding UTF8
Restart-Service docker -ErrorAction SilentlyContinue
`, string(mirrorsJSON))
	} else {
		shell = "sh"
		cmd = fmt.Sprintf(`set -e
mkdir -p /etc/docker
CFG="/etc/docker/daemon.json"
if [ ! -f "$CFG" ]; then
    echo "{}" > "$CFG"
fi
python3 -c '
import json, sys
cfg_path = "/etc/docker/daemon.json"
mirrors = json.loads(sys.argv[1])
try:
    with open(cfg_path, "r") as f:
        data = json.load(f)
except Exception:
    data = {}
data["registry-mirrors"] = mirrors
with open(cfg_path, "w") as f:
    json.dump(data, f, indent=2)
' '%s' 2>/dev/null || perl -MJSON::PP -e '
my $path = "/etc/docker/daemon.json";
my $mirrors = decode_json($ARGV[0]);
my $data = {};
if (open my $fh, "<", $path) {
    local $/;
    eval { $data = decode_json(<$fh>); };
    close $fh;
}
$data->{"registry-mirrors"} = $mirrors;
open my $out, ">", $path;
print $out encode_json($data);
close $out;
' '%s' 2>/dev/null || {
    echo "{\"registry-mirrors\": %s}" > "$CFG"
}
if command -v systemctl >/dev/null 2>&1; then
    systemctl reload docker || systemctl restart docker || true
elif command -v service >/dev/null 2>&1; then
    service docker reload || service docker restart || true
fi
`, string(mirrorsJSON), string(mirrorsJSON), string(mirrorsJSON))
	}

	execID := randomToken(8)
	resultCh := make(chan *agentpb.ExecResult, 1)
	hub.SetRespHandler(execID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(execID, nil)
		if msg != nil {
			resultCh <- msg.GetExecResult()
		} else {
			resultCh <- nil
		}
	})

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:     execID,
				Shell:      shell,
				Command:    cmd,
				IsScript:   true,
				TimeoutSec: 30,
			},
		},
	})
	defer hub.SetRespHandler(execID, nil)

	select {
	case res := <-resultCh:
		if res == nil {
			h.recordAudit(c, "docker_mirrors", "host", hostID, "更新镜像加速失败(agent断开)", audit.RiskLow, audit.ResultFailed)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		if res.GetExitCode() != 0 {
			errStr := res.GetError()
			if errStr == "" {
				errStr = string(res.GetStderr())
			}
			h.recordAudit(c, "docker_mirrors", "host", hostID, fmt.Sprintf("更新镜像加速失败: %s", errStr), audit.RiskLow, audit.ResultFailed)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update daemon.json: " + errStr})
			return
		}
		h.recordAudit(c, "docker_mirrors", "host", hostID, fmt.Sprintf("配置 Docker 镜像加速: %v", cleanMirrors), audit.RiskLow, audit.ResultSuccess)
		c.JSON(http.StatusOK, gin.H{"ok": true, "mirrors": cleanMirrors})
	case <-time.After(35 * time.Second):
		h.recordAudit(c, "docker_mirrors", "host", hostID, "更新镜像加速超时", audit.RiskLow, audit.ResultFailed)
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "timeout updating mirrors on host"})
	}
}

// ---- Agent Upgrade ---------------------------------------------------

func (h *handlers) upgradeAgent(c *gin.Context) {
	agentID := c.Param("id")
	hub := h.reg.Hub(agentID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}
	a := h.reg.GetAgent(agentID)
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}

	var body struct {
		Version string `json:"version"`
		Sha256  string `json:"sha256"`
	}
	_ = c.ShouldBindJSON(&body)
	if body.Version == "" {
		body.Version = agentTargetVersion()
	}

	// Build the binary download URL: -public-url when configured,
	// else the server's own address from this request.
	osName := strings.ToLower(a.OS)
	if osName == "" {
		osName = "linux"
	}
	archName := strings.ToLower(a.Arch)
	if archName == "" {
		archName = "amd64"
	}
	binaryURL := fmt.Sprintf("%s/api/v1/agent/binary?os=%s&arch=%s",
		upgradeDownloadBase(c), osName, archName)

	// Collect upgrade progress messages.
	progressCh := make(chan *agentpb.UpgradeProgress, 16)
	hub.SetRespHandler("upgrade", func(msg *agentpb.AgentMessage) {
		if msg == nil {
			progressCh <- nil
			return
		}
		if p := msg.GetUpgradeProgress(); p != nil {
			progressCh <- p
		}
	})
	defer hub.SetRespHandler("upgrade", nil)

	// Suppress offline and reconnect alerts for this host during the upgrade window
	if h.alertMon != nil {
		h.alertMon.SetHostMaintenance(agentID, 180*time.Second, "agent_upgrade")
	}
	h.reg.SetAgentUpgrading(agentID, body.Version)

	if body.Sha256 == "" {
		// Prefer the release manifest: its sha256 matches the binaries in
		// /opt/watchman/bin that /api/v1/agent/binary actually serves.
		if sum, ok := manifestAgentSha256(osName, archName); ok {
			body.Sha256 = sum
		} else if sha, err := install.AgentBinarySha256("", osName, archName); err == nil {
			body.Sha256 = sha
		}
	}
	sig := signUpgrade(body.Version, body.Sha256)

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Upgrade{
			Upgrade: &agentpb.UpgradeRequest{
				Version:   body.Version,
				Url:       binaryURL,
				Sha256:    body.Sha256,
				Signature: sig,
			},
		},
	})

	// Stream progress until done or error.
	var lastProgress *agentpb.UpgradeProgress
	timeout := time.After(120 * time.Second)
loop:
	for {
		select {
		case p := <-progressCh:
			if p == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
				return
			}
			lastProgress = p
			if p.GetError() != "" {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": p.GetError(),
					"stage": p.GetStage(),
				})
				return
			}
			if p.GetStage() == "restarting" && p.GetProgress() >= 1 {
				break loop
			}
		case <-timeout:
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "upgrade timeout", "stage": lastProgress.GetStage()})
			return
		}
	}

	h.recordAudit(c, "agent_upgrade", "host", c.Param("id"),
		fmt.Sprintf("升级 Agent 到 %s", body.Version), audit.RiskMedium, audit.ResultSuccess)
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"version": body.Version,
		"message": "upgrade complete, agent restarting",
	})
}

// ---- Audit helper for user/policy/vault mutations ---------------------

// auditMutation wraps admin mutation route groups and records successful
// writes to the unified audit trail. GETs and failed requests are skipped;
// the action name is derived from the HTTP method + route pattern.
func (h *handlers) auditMutation() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if h.audit == nil || c.Request.Method == http.MethodGet || c.Writer.Status() >= 400 {
			return
		}
		rule, ok := deriveMutationAction(c.Request.Method, c.FullPath())
		if !ok {
			return
		}
		targetType, targetID := rule.targetType, ""
		if targetType == "" {
			// Legacy heuristic for the earliest rules: user routes carry
			// :username, host routes carry :id, the rest fall back to :name.
			targetType, targetID = "system", c.Param("username")
			if id := c.Param("id"); id != "" {
				targetType, targetID = "host", id
			}
			if targetID == "" {
				targetID = c.Param("name")
			}
		} else if rule.targetParam != "" {
			targetID = c.Param(rule.targetParam)
		}
		risk := rule.risk
		if risk == "" {
			risk = audit.RiskMedium
		}
		h.recordAudit(c, rule.action, targetType, targetID,
			c.Request.Method+" "+c.FullPath(), risk, audit.ResultSuccess)
	}
}

// mutationRule maps one mutating route to its audit action. targetType pins
// the audited target type (empty = legacy param heuristic); targetParam names
// the gin param holding the target ID (empty = heuristic, or none when
// targetType is set); risk overrides the default medium level (empty = medium).
type mutationRule struct {
	method, pattern, action string
	targetType, targetParam  string
	risk                    string
}

// deriveMutationAction maps an HTTP method + gin route pattern to a concrete
// audit action name.
func deriveMutationAction(method, pattern string) (mutationRule, bool) {
	rules := []mutationRule{
		{http.MethodPost, "/api/v1/auth/logout", "logout", "system", "", audit.RiskLow},
		{http.MethodPost, "/api/v1/users", "user_create", "", "", ""},
		{http.MethodPut, "/api/v1/users/:username/password", "user_password", "", "", ""},
		{http.MethodPut, "/api/v1/users/:username/role", "user_role", "", "", ""},
		{http.MethodPost, "/api/v1/users/:username/reset-password", "user_reset_password", "", "", ""},
		{http.MethodDelete, "/api/v1/users/:username", "user_delete", "", "", audit.RiskHigh},
		{http.MethodPut, "/api/v1/policy/command", "policy_update", "", "", audit.RiskHigh},
		{http.MethodPost, "/api/v1/hosts/:id/apps/install", "app_install", "", "", ""},
		{http.MethodDelete, "/api/v1/hosts/:id/apps/:name", "app_uninstall", "", "", ""},
		{http.MethodPost, "/api/v1/hosts/:id/docker/install-script", "docker_install", "", "", audit.RiskHigh},
		{http.MethodPut, "/api/v1/me/settings", "user_settings_update", "", "", ""},
		{http.MethodPut, "/api/v1/settings", "system_settings_update", "", "", ""},
		// Control-plane backup / restore: a restore rewrites the whole dataset,
		// so it is recorded as a high-risk action.
		{http.MethodPost, "/api/v1/system/backup", "system_backup", "", "", ""},
		{http.MethodDelete, "/api/v1/system/backups/:name", "system_backup_delete", "", "", audit.RiskHigh},
		{http.MethodPost, "/api/v1/system/backups/:name/restore", "system_restore", "", "", audit.RiskHigh},
		{http.MethodPost, "/api/v1/system/restore", "system_restore", "", "", audit.RiskHigh},
		// App lifecycle (Dokploy-style): deploy/rollback/start/stop/restart
		// change the running state, so they are high-risk.
		{http.MethodPost, "/api/v1/apps", "app_create", "app", "", ""},
		{http.MethodPut, "/api/v1/apps/:id", "app_update", "app", "id", ""},
		{http.MethodDelete, "/api/v1/apps/:id", "app_delete", "app", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/deploy", "app_deploy", "app", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/rollback", "app_rollback", "app", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/stop", "app_stop", "app", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/start", "app_start", "app", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/restart", "app_restart", "app", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/webhook/sync", "app_webhook_sync", "app", "id", audit.RiskHigh},
		// Reverse-proxy domain binding: traffic-affecting, high-risk.
		{http.MethodPut, "/api/v1/apps/:id/proxy", "app_proxy_bind", "app", "id", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/apps/:id/proxy", "app_proxy_unbind", "app", "id", audit.RiskHigh},
		// Certificate hub: key material is sensitive.
		{http.MethodPut, "/api/v1/certs/config", "cert_config_update", "cert", "", ""},
		{http.MethodPost, "/api/v1/certs/accounts", "cert_account_create", "cert", "", ""},
		{http.MethodPut, "/api/v1/certs/accounts/:id", "cert_account_update", "cert", "id", ""},
		{http.MethodDelete, "/api/v1/certs/accounts/:id", "cert_account_delete", "cert", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/issue", "cert_issue", "cert", "", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/manual/:id/confirm", "cert_manual_dns_confirm", "cert", "id", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/certs/manual/:id", "cert_manual_dns_cancel", "cert", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/import", "cert_import", "cert", "", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/:id/renew", "cert_renew", "cert", "id", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/certs/:id", "cert_delete", "cert", "id", audit.RiskHigh},
		// File management on hosts: removal is destructive.
		{http.MethodPost, "/api/v1/hosts/:id/files/mkdir", "file_mkdir", "host", "id", ""},
		{http.MethodPost, "/api/v1/hosts/:id/files/move", "file_move", "host", "id", ""},
		{http.MethodPost, "/api/v1/hosts/:id/files/copy", "file_copy", "host", "id", ""},
		{http.MethodDelete, "/api/v1/hosts/:id/files", "file_remove", "host", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/hosts/:id/files/upload", "file_upload", "host", "id", ""},
		// Security scan trigger: active probing on the host.
		{http.MethodPost, "/api/v1/hosts/:id/scans", "scan_trigger", "host", "id", ""},
		// Snapshot / backup jobs: a restore overwrites live data.
		{http.MethodPost, "/api/v1/backups/jobs", "backup_job_create", "backup", "", ""},
		{http.MethodPut, "/api/v1/backups/jobs/:id", "backup_job_update", "backup", "id", ""},
		{http.MethodDelete, "/api/v1/backups/jobs/:id", "backup_job_delete", "backup", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/backups/jobs/:id/run", "backup_job_run", "backup", "id", ""},
		{http.MethodPost, "/api/v1/backups/jobs/:id/archives/:archiveID/restore", "backup_archive_restore", "backup", "archiveID", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/backups/jobs/:id/archives/:archiveID", "backup_archive_delete", "backup", "archiveID", audit.RiskHigh},
		{http.MethodPost, "/api/v1/backups/s3-targets", "backup_s3_create", "backup", "", ""},
		{http.MethodPut, "/api/v1/backups/s3-targets/:id", "backup_s3_update", "backup", "id", ""},
		{http.MethodDelete, "/api/v1/backups/s3-targets/:id", "backup_s3_delete", "backup", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/backups/s3-targets/:id/test", "backup_s3_test", "backup", "id", audit.RiskLow},
		// Git provider credentials: token material is sensitive.
		{http.MethodPost, "/api/v1/git/github/token", "git_token_set", "system", "", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/git/github", "git_account_delete", "system", "", audit.RiskHigh},
		// Host groups + per-group grants: authorization changes are high-risk.
		{http.MethodPost, "/api/v1/groups", "group_create", "group", "", ""},
		{http.MethodPatch, "/api/v1/groups/:id", "group_update", "group", "id", ""},
		{http.MethodDelete, "/api/v1/groups/:id", "group_delete", "group", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/groups/:id/users", "group_grant", "group", "id", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/groups/:id/users/:username", "group_revoke", "group", "id", audit.RiskHigh},
		// Saved-command library.
		{http.MethodPost, "/api/v1/commands", "command_create", "command", "", ""},
		{http.MethodPatch, "/api/v1/commands/:id", "command_update", "command", "id", ""},
		{http.MethodDelete, "/api/v1/commands/:id", "command_delete", "command", "id", audit.RiskHigh},
		// Alerts: rules/webhook changes and event evidence destruction.
		{http.MethodPost, "/api/v1/alerts/rules", "alert_rule_create", "alert", "", ""},
		{http.MethodPut, "/api/v1/alerts/rules/:id", "alert_rule_update", "alert", "id", ""},
		{http.MethodDelete, "/api/v1/alerts/rules/:id", "alert_rule_delete", "alert", "id", audit.RiskHigh},
		{http.MethodPost, "/api/v1/alerts/events/ack-all", "alert_events_ack_all", "alert", "", audit.RiskLow},
		{http.MethodPost, "/api/v1/alerts/events/:id/ack", "alert_event_ack", "alert", "id", audit.RiskLow},
		{http.MethodDelete, "/api/v1/alerts/events/:id", "alert_event_delete", "alert", "id", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/alerts/events", "alert_events_clear", "alert", "", audit.RiskHigh},
		{http.MethodPut, "/api/v1/alerts/webhook", "alert_webhook_update", "alert", "", ""},
		{http.MethodPost, "/api/v1/alerts/webhook/test", "alert_webhook_test", "alert", "", audit.RiskLow},
		// AI assistant config: model/endpoint credentials.
		{http.MethodPut, "/api/v1/ai/config", "ai_config_update", "system", "", ""},
	}
	for _, r := range rules {
		if method == r.method && pattern == r.pattern {
			return r, true
		}
	}
	// Vault credential mutations share one pattern family.
	if strings.HasPrefix(pattern, "/api/v1/vault") {
		return mutationRule{method: method, pattern: pattern, action: "vault_op", risk: audit.RiskHigh}, true
	}
	return mutationRule{}, false
}

// ---- Sessions / Audit ------------------------------------------------

func (h *handlers) listSessions(c *gin.Context) {
	if h.sess == nil {
		c.JSON(http.StatusOK, gin.H{"data": []any{}, "total": 0})
		return
	}
	list := h.sess.List()
	total := len(list)
	// Optional paging. Without page_size the full list is returned, so existing
	// callers keep working; total always reports the unpaged count so the client
	// can render a page count.
	offset, _ := strconv.Atoi(c.Query("offset"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	if pageSize > 0 {
		if offset < 0 {
			offset = 0
		}
		if offset > total {
			offset = total
		}
		end := offset + pageSize
		if end > total {
			end = total
		}
		list = list[offset:end]
	}
	c.JSON(http.StatusOK, gin.H{"data": list, "total": total})
}

func (h *handlers) getSession(c *gin.Context) {
	if h.sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	sess, ok := h.sess.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": sess})
}

// getRecording fetches the asciinema cast file for a session. If the recording
// is already cached locally, it's served directly; otherwise the server pulls
// it from the agent via the file-read channel (agent writes cast files to its
// temp dir as <session_id>.cast).
func (h *handlers) getRecording(c *gin.Context) {
	if h.sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	sid := c.Param("id")
	sess, ok := h.sess.Get(sid)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	// Serve cached recording if available.
	if path := h.sess.RecordingPath(sid); path != "" {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			normalized := normalizeCastRecording(raw, sess.Duration)
			c.Data(http.StatusOK, "application/json", normalized)
			return
		}
	}

	// Fetch from agent: the agent records to <recordDir>/<sid>.cast. We request
	// it via the file-read mechanism, but the agent's record dir is not a known
	// path to the caller. Instead, we send a FileOp read with a sentinel path
	// that the agent interprets as "send my recording for this session".
	hub := h.reg.Hub(sess.AgentID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline, recording not cached"})
		return
	}

	// Read the cast file from the agent. The agent's shell manager writes to
	// recordDir/<sid>.cast. We don't know the absolute path on the agent, so
	// we use a special protocol: send FileOp with op="read" and a path that
	// starts with "watchman-record:" — the agent's file handler recognizes this
	// prefix and resolves it to the recording directory.
	opID := randomToken(8)
	type chunkOrAck struct {
		chunk *agentpb.FileChunk
		ack   *agentpb.Ack
	}
	resultCh := make(chan chunkOrAck, 32)
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		if msg == nil {
			resultCh <- chunkOrAck{}
			return
		}
		if fc := msg.GetFileChunk(); fc != nil {
			resultCh <- chunkOrAck{chunk: fc}
			return
		}
		if ack := msg.GetAck(); ack != nil {
			resultCh <- chunkOrAck{ack: ack}
		}
	})
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_FileOp{
			FileOp: &agentpb.FileOp{
				OpId: opID,
				Op:   "read",
				Path: "watchman-record:" + sid,
			},
		},
	})

	// Collect chunks until EOF or an error Ack.
	var allData []byte
	timeout := time.After(15 * time.Second)
	defer hub.SetRespHandler(opID, nil)
loop:
	for {
		select {
		case r := <-resultCh:
			if r.ack != nil && !r.ack.GetOk() {
				c.JSON(http.StatusNotFound, gin.H{"error": r.ack.GetError()})
				return
			}
			if r.chunk != nil {
				allData = append(allData, r.chunk.GetData()...)
				if r.chunk.GetEof() {
					break loop
				}
			}
			// nil message (agent disconnected)
			if r.chunk == nil && r.ack == nil {
				break loop
			}
		case <-timeout:
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "recording fetch timeout"})
			return
		}
	}

	if len(allData) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "empty recording"})
		return
	}

	// Cache it.
	_ = h.sess.SaveRecording(sid, allData)
	normalized := normalizeCastRecording(allData, sess.Duration)
	c.Data(http.StatusOK, "application/json", normalized)
}

// normalizeCastRecording inspects an asciicast v2 file, ensures the header contains
// the session duration, and appends a closing event timestamp if missing, so the
// replay player's scrubber accurately covers the entire session lifetime rather
// than stopping after the last keyboard activity.
func normalizeCastRecording(raw []byte, durationSec int64) []byte {
	if len(raw) == 0 {
		return raw
	}
	lines := bytes.Split(raw, []byte("\n"))
	if len(lines) == 0 {
		return raw
	}

	// Parse header
	var header map[string]any
	if err := json.Unmarshal(lines[0], &header); err != nil {
		return raw
	}

	if durationSec > 0 {
		header["duration"] = float64(durationSec)
		if hb, err := json.Marshal(header); err == nil {
			lines[0] = hb
		}
	}

	// Find the timestamp of the last event
	var lastEventTime float64
	for i := len(lines) - 1; i >= 1; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var evt []any
		if err := json.Unmarshal(line, &evt); err == nil && len(evt) >= 2 {
			if t, ok := evt[0].(float64); ok {
				lastEventTime = t
				break
			}
		}
	}

	// If the recorded events cut off earlier than the session duration, append
	// an EOF close frame so the player timeline reaches the full duration.
	if durationSec > 0 && float64(durationSec) > lastEventTime {
		closeEvt := []any{float64(durationSec), "o", ""}
		if cb, err := json.Marshal(closeEvt); err == nil {
			// Find trailing non-empty line
			var filtered [][]byte
			for _, l := range lines {
				if len(bytes.TrimSpace(l)) > 0 {
					filtered = append(filtered, l)
				}
			}
			filtered = append(filtered, cb)
			return append(bytes.Join(filtered, []byte("\n")), '\n')
		}
	}

	return raw
}

func (h *handlers) deleteSession(c *gin.Context) {
	if h.sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	sid := c.Param("id")
	if err := h.sess.Delete(sid); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Deleting a recording destroys audit evidence: always log it.
	h.recordAudit(c, "session_delete", "session", sid, "删除会话/录像", audit.RiskHigh, audit.ResultSuccess)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- Health ----------------------------------------------------------

func (h *handlers) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"ok":                   true,
		"agents":               len(h.reg.ListAgents()),
		"version":              version.Get(),
		"agent_latest_version": agentTargetVersion(),
		"os":                   runtime.GOOS,
		"arch":                 runtime.GOARCH,
		"online_agents":        h.reg.CountOnline(),
	})
}

// versionInfo exposes build metadata and the agent-upgrade source of
// truth: which agent version upgrades target, whether it comes from the
// release manifest, and the public key agents can pin (-upgrade-pubkey)
// to verify upgrade signatures.
func (h *handlers) versionInfo(c *gin.Context) {
	panelTZ := ""
	if h.settings != nil {
		panelTZ = h.settings.Timezone()
	}
	c.JSON(http.StatusOK, gin.H{
		"server_version":       version.Get(),
		"server_commit":        version.Commit,
		"build_time":           version.BuildTime,
		"agent_target_version": agentTargetVersion(),
		"agent_manifest":       agentRelease != nil,
		"upgrade_pubkey":       UpgradePubKeyHex,
		"public_url":           publicURL,
		// Panel timezone (IANA name) so every client renders times in the
		// same zone; "" = clients fall back to their local timezone.
		"timezone": panelTZ,
	})
}

// ---- Helpers ---------------------------------------------------------

func (h *handlers) toDTO(a *rpc.Agent) HostDTO {
	tags := a.Tags
	if tags == nil {
		tags = []string{}
	}
	targetVer := agentTargetVersion()
	dto := HostDTO{
		ID:                 a.ID,
		Hostname:           a.Hostname,
		OS:                 a.OS,
		Arch:               a.Arch,
		Distro:             a.Distro,
		Version:            a.Version,
		AgentLatestVersion: targetVer,
		AgentOutdated:      a.Status == "online" && a.Version != "" && a.Version != targetVer,
		Status:             a.Status,
		LastSeen:           a.LastSeen.Format("2006-01-02T15:04:05Z07:00"),
		Registered:         a.Registered.Format("2006-01-02T15:04:05Z07:00"),
		Group:              a.Group,
		Tags:               tags,
		Uptime:             a.Uptime,
		CPUCores:           a.CPUCores,
		MemTotal:           a.MemTotal,
		InternalIP:         a.InternalIP,
		PublicIP:           a.PublicIP,
		Location:           a.Location,
		Price:              a.Price,
		Currency:           a.Currency,
		BillingCycle:       a.BillingCycle,
		ExpiresAt:          a.ExpiresAt,
		AutoRenewal:        a.AutoRenewal,
		TrafficLimitGB:     a.TrafficLimitGB,
		TrafficCalcType:    a.TrafficCalcType,
		TrafficResetDay:    a.TrafficResetDay,
		RenewalURL:         a.RenewalURL,
		Notes:              a.Notes,
	}
	// Surface the real OS uptime and latest metrics
	if hub := h.reg.Hub(a.ID); hub != nil {
		if m := hub.LastMetrics(); m != nil {
			if m.GetUptime() > 0 {
				dto.Uptime = m.GetUptime()
			}
			if m.GetMemTotal() > 0 {
				dto.MemTotal = m.GetMemTotal()
			}
			dto.CpuModel = m.GetCpuModel()
			dto.Load1 = m.GetLoad1()
			dto.MonthRx = m.GetMonthRx()
			dto.MonthTx = m.GetMonthTx()
			if m.GetSwapTotal() > 0 {
				dto.SwapUsage = float64(m.GetSwapUsed()) / float64(m.GetSwapTotal()) * 100
			}
		}
	}
	if upg := h.reg.GetAgentUpgrade(a.ID); upg != nil {
		if upg.Stage != "error" {
			dto.Upgrading = true
		}
		dto.UpgradeStage = upg.Stage
		dto.UpgradeTarget = upg.TargetVersion
		dto.UpgradeError = upg.Error
	}
	return dto
}

func toDTO(a *rpc.Agent) HostDTO {
	tags := a.Tags
	if tags == nil {
		tags = []string{}
	}
	targetVer := agentTargetVersion()
	return HostDTO{
		ID:                 a.ID,
		Hostname:           a.Hostname,
		OS:                 a.OS,
		Arch:               a.Arch,
		Distro:             a.Distro,
		Version:            a.Version,
		AgentLatestVersion: targetVer,
		AgentOutdated:      a.Status == "online" && a.Version != "" && a.Version != targetVer,
		Status:             a.Status,
		LastSeen:           a.LastSeen.Format("2006-01-02T15:04:05Z07:00"),
		Registered:         a.Registered.Format("2006-01-02T15:04:05Z07:00"),
		Group:              a.Group,
		Tags:               tags,
		Uptime:             a.Uptime,
		CPUCores:           a.CPUCores,
		MemTotal:           a.MemTotal,
		InternalIP:         a.InternalIP,
		PublicIP:           a.PublicIP,
		Location:           a.Location,
	}
}

func requestLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		log.Debug("http",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
		)
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// avoid unused import
var _ = fmt.Sprintf

// UpgradePayload defines the JSON body for uploading a new server binary.
type UpgradePayload struct {
	Data   string `json:"data"`   // Base64 encoded binary
	Sha256 string `json:"sha256"` // Optional expected SHA-256
}

// systemUpgrade accepts either a multipart/form-data upload or a base64-encoded binary,
// validates size, SHA-256 (if provided), ELF/PE format, and host architecture, executes
// a dry-run smoke test, and replaces the running watchman-server binary atomically.
func (h *handlers) systemUpgrade(c *gin.Context) {
	self, err := os.Executable()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	target, err := filepath.EvalSymlinks(self)
	if err != nil {
		target = self
	}

	dir := filepath.Dir(target)
	tmp := filepath.Join(dir, fmt.Sprintf("watchman-server.upgrade.%d", time.Now().UnixNano()))

	var fileSize int64
	var expectedSha string
	contentType := c.GetHeader("Content-Type")

	if strings.Contains(contentType, "multipart/form-data") {
		expectedSha = strings.TrimSpace(c.PostForm("sha256"))
		fileHeader, err := c.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请选择上传的文件: " + err.Error()})
			return
		}
		fileSize = fileHeader.Size
		src, err := fileHeader.Open()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "打开上传文件失败: " + err.Error()})
			return
		}
		defer src.Close()

		dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("创建临时文件失败: %v", err)})
			return
		}

		if _, err := io.Copy(dst, src); err != nil {
			_ = dst.Close()
			_ = os.Remove(tmp)
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("写入文件失败: %v", err)})
			return
		}
		_ = dst.Close()
	} else {
		// Fallback: JSON base64
		var body UpgradePayload
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的上传参数"})
			return
		}
		expectedSha = strings.TrimSpace(body.Sha256)
		raw, err := base64.StdEncoding.DecodeString(body.Data)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base64 data"})
			return
		}
		fileSize = int64(len(raw))
		if err := os.WriteFile(tmp, raw, 0755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("写入临时文件失败: %v", err)})
			return
		}
	}

	// 1. Calculate actual SHA-256 and verify against expected SHA-256 if provided
	actualSha, err := binval.FileSha256(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("计算二进制 SHA-256 失败: %v", err)})
		return
	}
	if expectedSha != "" && !strings.EqualFold(actualSha, expectedSha) {
		_ = os.Remove(tmp)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("SHA-256 校验失败 (期望: %s, 实际: %s)，上传二进制可能已被篡改或不完整", expectedSha, actualSha)})
		return
	}

	// 2. Validate binary format and architecture compatibility
	if err := binval.ValidateFormat(tmp, runtime.GOOS, runtime.GOARCH, binval.MinServerBinarySize); err != nil {
		_ = os.Remove(tmp)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("二进制格式/架构安全校验失败: %v", err)})
		return
	}

	// 3. Smoke-test dry run (-version) to verify runnability
	smokeOut, err := binval.SmokeTest(c.Request.Context(), tmp, runtime.GOOS, runtime.GOARCH, "watchman-server")
	if err != nil {
		_ = os.Remove(tmp)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("二进制可执行性自检失败: %v", err)})
		return
	}

	// Safety backup of existing binary
	bakPath := filepath.Join(dir, fmt.Sprintf("watchman-server.bak.%d", time.Now().Unix()))
	_ = os.Rename(target, bakPath)

	// Atomic rename to replace the running binary
	if err := os.Rename(tmp, target); err != nil {
		// Rollback if possible
		_ = os.Rename(bakPath, target)
		_ = os.Remove(tmp)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("原子替换二进制失败: %v", err)})
		return
	}
	_ = os.Chmod(target, 0755)

	// Broadcast maintenance notice to all agents beforehand
	h.reg.BroadcastMaintenance("server_upgrade", 180)

	h.recordAudit(c, "update", "system", "server_binary",
		fmt.Sprintf("上传新版本控制端二进制并验证通过 (大小: %d 字节, SHA-256: %s, 自检: %s)", fileSize, actualSha, smokeOut),
		audit.RiskHigh, audit.ResultSuccess)

	c.JSON(http.StatusOK, gin.H{
		"ok":       true,
		"message":  "控制端二进制已通过格式、架构与运行自检，并成功安全替换！系统已向全网 Agent 下发维护预告，请点击平滑重启生效。",
		"path":     target,
		"size":     fileSize,
		"sha256":   actualSha,
		"bak_path": bakPath,
	})
}

// restartService restarts the control-plane process.
//
// Preferred path: `systemctl restart watchman-server`. The unit file written
// by install.sh carries Restart=always, but more importantly systemd enforces
// the stop with SIGKILL after TimeoutStopSec — so the restart is guaranteed
// even if the in-process graceful shutdown wedges (e.g. an old binary whose
// gRPC GracefulStop blocks forever on persistent agent streams; the process
// then never exits and a self-sent SIGINT alone would leave the panel dead).
//
// Fallback: when systemctl is unavailable or fails (non-systemd deployments,
// renamed units), send SIGINT to ourselves as before and rely on whatever
// supervisor is in place.
func restartService(log *slog.Logger) {
	go func() {
		time.Sleep(1 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, "systemctl", "--no-block", "restart", "watchman-server").Run(); err != nil {
			if log != nil {
				log.Warn("systemctl restart failed, falling back to self SIGINT", "err", err)
			}
			if p, perr := os.FindProcess(os.Getpid()); perr == nil {
				_ = p.Signal(os.Interrupt)
			}
			return
		}
		if log != nil {
			log.Info("systemctl restart watchman-server issued")
		}
	}()
}

// systemRestart initiates graceful server restart under systemd / supervisor.
func (h *handlers) systemRestart(c *gin.Context) {
	h.reg.BroadcastMaintenance("server_restart", 120)
	h.recordAudit(c, "restart", "system", "watchman-server",
		"触发控制端平滑重启流程 (maintenance handshake)",
		audit.RiskHigh, audit.ResultSuccess)

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": "已广播停机维护预告，正在触发控制端平滑重载...",
	})

	restartService(h.log)
}

// checkUpdate queries GitHub Releases to determine whether an update is available.
func (h *handlers) checkUpdate(c *gin.Context) {
	force := c.Query("force") == "true" || c.Query("force") == "1"
	joinBeta := false
	if h.settings != nil {
		joinBeta = h.settings.JoinBetaProgram()
	}
	if betaParam := c.Query("beta"); betaParam != "" {
		joinBeta = betaParam == "true" || betaParam == "1"
	}

	info, err := release.CheckUpdate(c.Request.Context(), version.Get(), joinBeta, force)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "检查更新失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

// onlineUpgrade downloads the official release package matching target_version or latest,
// verifies SHA-256 against CHECKSUMS.txt, extracts and replaces watchman-server,
// updates manifest.json and agent binaries, broadcasts maintenance, and restarts.
func (h *handlers) onlineUpgrade(c *gin.Context) {
	var body struct {
		TargetVersion string `json:"target_version"`
	}
	_ = c.ShouldBindJSON(&body)

	joinBeta := false
	if h.settings != nil {
		joinBeta = h.settings.JoinBetaProgram()
	}

	// Retrieve release info (force refresh to ensure latest download URL and checksums).
	info, err := release.CheckUpdate(c.Request.Context(), version.Get(), joinBeta, true)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "获取最新版本信息失败: " + err.Error()})
		return
	}

	if body.TargetVersion != "" && body.TargetVersion != info.LatestVersion {
		info.LatestVersion = body.TargetVersion
	}

	if info.AssetURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("未找到适用于当前系统/架构 (%s/%s) 的官方安装包", runtime.GOOS, runtime.GOARCH)})
		return
	}

	mPath := ManifestPath()
	if err := release.PerformOnlineUpgrade(c.Request.Context(), info.AssetURL, info.ChecksumsURL, info.LatestVersion, h.reg, mPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("在线升级失败: %v", err)})
		return
	}

	h.recordAudit(c, "upgrade", "system", "watchman-server",
		fmt.Sprintf("控制端一键在线升级至 %s (渠道: %s)", info.LatestVersion, info.Channel),
		audit.RiskHigh, audit.ResultSuccess)

	c.JSON(http.StatusOK, gin.H{
		"ok":             true,
		"message":        fmt.Sprintf("控制端已成功下载并升级至 %s，正在触发平滑重载...", info.LatestVersion),
		"target_version": info.LatestVersion,
	})

	restartService(h.log)
}

// upgradeAgentsBatch pushes an upgrade to several hosts at once. Every target
// gets a maintenance window first so the restart does not page anyone.
func (h *handlers) upgradeAgentsBatch(c *gin.Context) {
	var body struct {
		HostIDs []string `json:"host_ids"` // optional; empty = all outdated online hosts
		Force   bool     `json:"force"`    // re-push even to hosts already on the current version
	}
	_ = c.ShouldBindJSON(&body)

	allAgents := h.reg.ListAgents()
	var targets []*rpc.Agent

	if len(body.HostIDs) > 0 {
		targetMap := make(map[string]bool)
		for _, id := range body.HostIDs {
			targetMap[id] = true
		}
		for _, a := range allAgents {
			if targetMap[a.ID] && a.Status == "online" {
				targets = append(targets, a)
			}
		}
	} else {
		for _, a := range allAgents {
			// Force is needed for dev builds, where every binary reports the
			// same version and nothing would ever look outdated.
			if a.Status != "online" {
				continue
			}
			if body.Force || a.Version != agentTargetVersion() {
				targets = append(targets, a)
			}
		}
	}

	if len(targets) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"message": "没有需要升级的在线 Agent 主机",
			"count":   0,
		})
		return
	}

	// Agent-facing download base: -public-url when configured, else this
	// request's own address. (Resolves the old server.public_url TODO: a
	// client-controlled Host header must not be the only source.)
	downloadBase := upgradeDownloadBase(c)

	dispatched := make([]string, 0, len(targets))
	shaCache := make(map[string]string)
	targetVersion := agentTargetVersion()
	for _, a := range targets {
		hub := h.reg.Hub(a.ID)
		if hub == nil {
			continue
		}

		// Suppress offline/online alert noise during the upgrade
		if h.alertMon != nil {
			h.alertMon.SetHostMaintenance(a.ID, 180*time.Second, "agent_upgrade")
		}
		h.reg.SetAgentUpgrading(a.ID, targetVersion)

		osName := strings.ToLower(a.OS)
		if osName == "" {
			osName = "linux"
		}
		archName := strings.ToLower(a.Arch)
		if archName == "" {
			archName = "amd64"
		}
		binaryURL := fmt.Sprintf("%s/api/v1/agent/binary?os=%s&arch=%s", downloadBase, osName, archName)

		key := osName + "/" + archName
		sha256Val, ok := shaCache[key]
		if !ok {
			// Prefer the release manifest: its sha256 matches the binaries
			// in /opt/watchman/bin that /api/v1/agent/binary serves.
			if sum, ok := manifestAgentSha256(osName, archName); ok {
				sha256Val = sum
			} else if sha, err := install.AgentBinarySha256("", osName, archName); err == nil {
				sha256Val = sha
			} else {
				h.log.Warn("batch upgrade: binary sha256 not found for agent", "agent_id", a.ID, "os", osName, "arch", archName, "err", err)
			}
			shaCache[key] = sha256Val
		}
		sig := signUpgrade(targetVersion, sha256Val)

		hub.Send(&agentpb.ServerMessage{
			Payload: &agentpb.ServerMessage_Upgrade{
				Upgrade: &agentpb.UpgradeRequest{
					Version:   targetVersion,
					Url:       binaryURL,
					Sha256:    sha256Val,
					Signature: sig,
				},
			},
		})
		dispatched = append(dispatched, a.Hostname)
	}

	h.recordAudit(c, "batch_upgrade", "system", "agents",
		fmt.Sprintf("批量升级 %d 台在线 Agent 到最新版本 %s", len(dispatched), targetVersion),
		audit.RiskMedium, audit.ResultSuccess)

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": fmt.Sprintf("已成功向 %d 台主机下发升级任务，已自动开启 3 分钟维护静默期", len(dispatched)),
		"count":   len(dispatched),
		"hosts":   dispatched,
	})
}
