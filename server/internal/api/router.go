// Package api implements the control server's REST API.
package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/ai"
	"watchman/server/internal/alert"
	"watchman/server/internal/apps"
	"watchman/server/internal/auth"
	"watchman/server/internal/metrics"
	"watchman/server/internal/policy"
	"watchman/server/internal/rpc"
	"watchman/server/internal/scan"
	"watchman/server/internal/session"
	"watchman/server/internal/vault"
	"watchman/server/internal/ws"

	"github.com/gin-gonic/gin"
)

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
}

// Router builds the gin engine with all routes mounted under /api/v1.
// authStore may be nil for a degraded (token-less) mode used only in tests;
// in production it is always provided. sessStore tracks terminal sessions
// for audit (may be nil to disable auditing).
func Router(reg *rpc.Registry, log *slog.Logger, authStore *auth.Store, sessStore *session.Store, alertStore *alert.Store, vaultStore *vault.Store, aiAssistant *ai.Assistant, metricsStore *metrics.Store, scanStore *scan.Store, policyStore *policy.Store) *gin.Engine {
	if log == nil {
		log = slog.Default()
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), requestLogger(log))

	v1 := r.Group("/api/v1")
	h := &handlers{reg: reg, log: log, sess: sessStore, auth: authStore, metrics: metricsStore, policy: policyStore}

	// ---- Public routes (no auth) ----
	// Auth login.
	if authStore != nil {
		ah := auth.NewHandlers(authStore)
		v1.POST("/auth/login", ah.Login)
	}

	// Enroll (generates a one-time token; the install script uses it).
	// Public by design (AGENTS.md B.4): one-click binding flow.
	v1.POST("/hosts/enroll", h.enroll)

	// Public share token info lookup for collaborative terminal guests
	v1.GET("/terminals/share/:token", h.getShareInfo)

	// ---- Authenticated routes ----
	authed := v1.Group("", auth.Middleware(authStore))

	// Hosts.
	authed.GET("/hosts", h.listHosts)
	authed.GET("/hosts/:id", h.getHost)
	authed.DELETE("/hosts/:id", h.deleteHost)
	authed.PUT("/hosts/:id/group", h.setHostGroup)
	authed.PUT("/hosts/:id/tags", h.setHostTags)

	// Terminal.
	authed.POST("/hosts/:id/terminals", h.openTerminal)
	authed.POST("/hosts/:id/terminals/:sid/share", h.shareTerminal)

	// Files.
	authed.GET("/hosts/:id/files", h.fileList)
	authed.GET("/hosts/:id/files/stat", h.fileStat)
	authed.POST("/hosts/:id/files/mkdir", h.fileMkdir)
	authed.POST("/hosts/:id/files/move", h.fileMove)
	authed.POST("/hosts/:id/files/copy", h.fileCopy)
	authed.DELETE("/hosts/:id/files", h.fileRemove)
	authed.GET("/hosts/:id/files/download", h.fileDownload)
	authed.POST("/hosts/:id/files/upload", h.fileUpload)

	// Exec (push command).
	authed.POST("/hosts/:id/exec", h.execCommand)
	authed.POST("/hosts/batch-exec", h.batchExec)

	// Metrics.
	authed.GET("/hosts/:id/metrics", h.getMetrics)
	authed.GET("/hosts/:id/metrics/history", h.getMetricsHistory)

	// SysInfo.
	authed.GET("/hosts/:id/sysinfo/:kind", h.getSysInfo)

	// Docker.
	authed.GET("/hosts/:id/docker/ps", h.dockerPs)
	authed.GET("/hosts/:id/docker/images", h.dockerImages)
	authed.GET("/hosts/:id/docker/all", h.dockerAll)
	authed.POST("/hosts/:id/docker/:op", h.dockerOp)

	// App Store & templates.
	apps.NewHandlers(reg).Register(authed)

	// Security scanning.
	if scanStore != nil {
		scan.NewHandlers(reg, scanStore, log).Register(authed)
	}

	// Sessions (audit / recordings).
	authed.GET("/sessions", h.listSessions)
	authed.GET("/sessions/:id", h.getSession)
	authed.GET("/sessions/:id/recording", h.getRecording)
	authed.DELETE("/sessions/:id", h.deleteSession)

	// Health.
	authed.GET("/system/health", h.health)

	// Agent upgrade.
	authed.POST("/hosts/:id/upgrade", auth.RequireRole(auth.RoleAdmin), h.upgradeAgent)

	// Reverse TCP tunnels (dynamic networking).
	h.registerTunnelRoutes(authed)

	// User management (admin-only for mutating routes).
	if authStore != nil {
		auth.NewHandlers(authStore).Register(authed.Group(""))
	}

	// Alerts (rules + events + webhook).
	if alertStore != nil {
		alert.NewHandlers(alertStore).Register(authed.Group(""))
	}

	// Credential vault (admin-only).
	if vaultStore != nil {
		vault.NewHandlers(vaultStore).Register(authed.Group(""))
	}

	// AI diagnostics.
	if aiAssistant != nil {
		ai.NewHandlers(aiAssistant).Register(authed.Group(""))
	}

	// High-risk command control (policy + audit).
	if policyStore != nil {
		policy.NewHandlers(policyStore).Register(authed.Group(""))
	}

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

	return r
}

type handlers struct {
	reg     *rpc.Registry
	log     *slog.Logger
	sess    *session.Store
	auth    *auth.Store
	metrics *metrics.Store
	policy  *policy.Store
}

func (h *handlers) enroll(c *gin.Context) {
	token := h.reg.IssueEnrollToken()
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	serverURL := fmt.Sprintf("%s://%s", scheme, c.Request.Host)
	c.JSON(http.StatusOK, gin.H{
		"enroll_token": token,
		"expires_in":   86400,
		"install":      fmt.Sprintf("curl -kfsSL '%s/install?token=%s' | sudo bash", serverURL, token),
		"install_win":  fmt.Sprintf("irm '%s/install?os_type=windows^&token=%s' | iex", serverURL, token),
	})
}

// ---- Hosts -----------------------------------------------------------

func (h *handlers) listHosts(c *gin.Context) {
	agents := h.reg.ListAgents()
	out := make([]HostDTO, 0, len(agents))
	for _, a := range agents {
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
	c.JSON(http.StatusOK, gin.H{"data": h.toDTO(a)})
}

func (h *handlers) deleteHost(c *gin.Context) {
	if err := h.reg.DeleteAgent(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
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

	sid := randomToken(12)
	ws.RegisterSession(sid, agentID, body.Shell)
	// Record the session start for audit.
	if h.sess != nil {
		operator := auth.Username(c)
		h.sess.Start(sid, agentID, a.Hostname, operator)
	}
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
		Command   string `json:"command"`
		Shell     string `json:"shell"`
		Timeout   int32  `json:"timeout_sec"`
		IsScript  bool   `json:"is_script"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// High-risk command policy check.
	if h.policy != nil {
		res := h.policy.Check(body.Command)
		username := auth.Username(c)
		hostID := c.Param("id")
		if !res.Allowed {
			h.policy.RecordAudit(policy.AuditEntry{
				Username: username, HostID: hostID, Command: body.Command, Shell: body.Shell,
				RiskLevel: res.RiskLevel, Result: "blocked", Reason: res.Reason,
			})
			c.JSON(http.StatusForbidden, gin.H{"error": res.Reason, "risk_level": res.RiskLevel, "matched_pattern": res.MatchedPattern})
			return
		}
		if res.NeedsConfirm && c.GetHeader("X-Confirm-Risk") != "true" {
			h.policy.RecordAudit(policy.AuditEntry{
				Username: username, HostID: hostID, Command: body.Command, Shell: body.Shell,
				RiskLevel: res.RiskLevel, Result: "denied", Reason: "未确认高危命令",
			})
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
	select {
	case res := <-resultCh:
		if res == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"exec_id":     res.GetExecId(),
			"exit_code":   res.GetExitCode(),
			"stdout":      string(res.GetStdout()),
			"stderr":      string(res.GetStderr()),
			"duration_ms": res.GetDurationMs(),
			"error":       res.GetError(),
		})
	case <-time.After(120 * time.Second):
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
	if h.policy != nil {
		res := h.policy.Check(body.Command)
		username := auth.Username(c)
		if !res.Allowed {
			h.policy.RecordAudit(policy.AuditEntry{
				Username: username, HostIDs: body.HostIDs, Command: body.Command, Shell: body.Shell,
				RiskLevel: res.RiskLevel, Result: "blocked", Reason: res.Reason,
			})
			c.JSON(http.StatusForbidden, gin.H{"error": res.Reason, "risk_level": res.RiskLevel, "matched_pattern": res.MatchedPattern})
			return
		}
		if res.NeedsConfirm && c.GetHeader("X-Confirm-Risk") != "true" {
			h.policy.RecordAudit(policy.AuditEntry{
				Username: username, HostIDs: body.HostIDs, Command: body.Command, Shell: body.Shell,
				RiskLevel: res.RiskLevel, Result: "denied", Reason: "未确认高危命令",
			})
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
		"ts":         m.GetTs(),
		"cpu_usage":  m.GetCpuUsage(),
		"mem_usage":  m.GetMemUsage(),
		"mem_total":  m.GetMemTotal(),
		"mem_used":   m.GetMemUsed(),
		"net_rx":     m.GetNetRx(),
		"net_tx":     m.GetNetTx(),
		"disk_read":  m.GetDiskRead(),
		"disk_write": m.GetDiskWrite(),
		"mounts":     mounts,
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
	ref := "sysinfo:" + kind
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
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_SysinfoQ{
			SysinfoQ: &agentpb.SysInfoQuery{Kind: kind},
		},
	})
	defer hub.SetRespHandler(ref, nil)
	select {
	case data := <-resultCh:
		if data == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no response"})
			return
		}
		// Return the raw JSON payload from the agent.
		c.Data(http.StatusOK, "application/json", data)
	case <-time.After(10 * time.Second):
		hub.SetRespHandler(ref, nil)
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "sysinfo timeout"})
	}
}

// ---- Docker ----------------------------------------------------------

func (h *handlers) dockerPs(c *gin.Context) {
	h.dockerCall(c, "ps", "")
}

func (h *handlers) dockerImages(c *gin.Context) {
	h.dockerCall(c, "images", "")
}

// dockerAll fetches containers + images in a single round trip to the agent
// (op "ps_images"), collapsing two HTTP calls into one for the dashboard.
func (h *handlers) dockerAll(c *gin.Context) {
	h.dockerCall(c, "ps_images", "")
}

func (h *handlers) dockerOp(c *gin.Context) {
	op := c.Param("op")
	var body struct {
		Container string `json:"container"`
		Image     string `json:"image"`
	}
	_ = c.ShouldBindJSON(&body)
	h.dockerCall(c, op, body.Container, body.Image)
}

func (h *handlers) dockerCall(c *gin.Context, op, container string, image ...string) {
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
	img := ""
	if len(image) > 0 {
		img = image[0]
	}
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_DockerOp{
			DockerOp: &agentpb.DockerOp{
				OpId:      opID,
				Op:        op,
				Container: container,
				Image:     img,
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

// dockerOpTimeout returns the HTTP-side wait budget for a docker op. Pulls
// can take minutes, so match the agent's longer budget instead of forcing a
// 504 at 30s while the agent keeps working.
func dockerOpTimeout(op string) time.Duration {
	if op == "pull" {
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

// ---- Agent Upgrade ---------------------------------------------------

func (h *handlers) upgradeAgent(c *gin.Context) {
	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	var body struct {
		Version string `json:"version"`
		Sha256  string `json:"sha256"`
	}
	_ = c.ShouldBindJSON(&body)

	// Build the binary download URL from the server's own address.
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	host := c.Request.Host
	binaryURL := fmt.Sprintf("%s://%s/api/v1/agent/binary?os=%s&arch=%s",
		scheme, host, "linux", "amd64") // agent OS; for MVP we assume linux agents

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

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Upgrade{
			Upgrade: &agentpb.UpgradeRequest{
				Version:   body.Version,
				Url:       binaryURL,
				Sha256:    body.Sha256,
				Signature: signUpgrade(body.Version, body.Sha256),
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
					"error":  p.GetError(),
					"stage":  p.GetStage(),
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

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"version": body.Version,
		"message": "upgrade complete, agent restarting",
	})
}

// ---- Sessions / Audit ------------------------------------------------

func (h *handlers) listSessions(c *gin.Context) {
	if h.sess == nil {
		c.JSON(http.StatusOK, gin.H{"data": []any{}, "total": 0})
		return
	}
	list := h.sess.List()
	c.JSON(http.StatusOK, gin.H{"data": list, "total": len(list)})
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
		c.File(path)
		return
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
	c.Data(http.StatusOK, "application/json", allData)
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
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- Health ----------------------------------------------------------

func (h *handlers) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"agents":  len(h.reg.ListAgents()),
		"version": "0.1.0-dev",
	})
}

// ---- Helpers ---------------------------------------------------------

func (h *handlers) toDTO(a *rpc.Agent) HostDTO {
	tags := a.Tags
	if tags == nil {
		tags = []string{}
	}
	dto := HostDTO{
		ID:         a.ID,
		Hostname:   a.Hostname,
		OS:         a.OS,
		Arch:       a.Arch,
		Distro:     a.Distro,
		Version:    a.Version,
		Status:     a.Status,
		LastSeen:   a.LastSeen.Format("2006-01-02T15:04:05Z07:00"),
		Registered: a.Registered.Format("2006-01-02T15:04:05Z07:00"),
		Group:      a.Group,
		Tags:       tags,
		Uptime:     a.Uptime,
		CPUCores:   a.CPUCores,
		MemTotal:   a.MemTotal,
		InternalIP: a.InternalIP,
		PublicIP:   a.PublicIP,
		Location:   a.Location,
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
		}
	}
	return dto
}

func toDTO(a *rpc.Agent) HostDTO {
	tags := a.Tags
	if tags == nil {
		tags = []string{}
	}
	return HostDTO{
		ID:         a.ID,
		Hostname:   a.Hostname,
		OS:         a.OS,
		Arch:       a.Arch,
		Distro:     a.Distro,
		Version:    a.Version,
		Status:     a.Status,
		LastSeen:   a.LastSeen.Format("2006-01-02T15:04:05Z07:00"),
		Registered: a.Registered.Format("2006-01-02T15:04:05Z07:00"),
		Group:      a.Group,
		Tags:       tags,
		Uptime:     a.Uptime,
		CPUCores:   a.CPUCores,
		MemTotal:   a.MemTotal,
		InternalIP: a.InternalIP,
		PublicIP:   a.PublicIP,
		Location:   a.Location,
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