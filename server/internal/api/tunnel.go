// Reverse TCP tunnel REST API (dynamic networking, §3.9).
//
// A tunnel is a port-forward through an agent: the control server listens
// on a loopback port and forwards connections to a target on the agent's
// network. Operators open tunnels to reach intranet services (databases,
// web consoles, SSH) on managed hosts without opening any inbound ports
// on those hosts.
package api

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"watchman/server/internal/audit"
	"watchman/server/internal/auth"
	"watchman/server/internal/rpc"

	"github.com/gin-gonic/gin"
)

func (h *handlers) registerTunnelRoutes(authed *gin.RouterGroup) {
	t := authed.Group("/hosts/:id/tunnels", auth.RequireRole(auth.RoleAdmin, auth.RoleOperator))
	t.POST("", h.openTunnel)
	t.GET("", h.listTunnels)
	t.DELETE("/:tid", h.closeTunnel)
}

func (h *handlers) tunnelCoord(c *gin.Context) bool {
	if h.reg.TunnelCoordinator() == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tunnel service disabled"})
		return false
	}
	return true
}

func (h *handlers) openTunnel(c *gin.Context) {
	if !h.tunnelCoord(c) {
		return
	}
	agentID := c.Param("id")
	a := h.reg.GetAgent(agentID)
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}
	if !h.canSeeHost(c, a) {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	var body struct {
		TargetHost string `json:"target_host"`
		TargetPort uint32 `json:"target_port"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	t, err := h.reg.TunnelCoordinator().Open(agentID, body.TargetHost, body.TargetPort)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}

	h.recordAudit(c, "tunnel_open", "host", agentID,
		fmt.Sprintf("打开反向 TCP 隧道到 %s:%d (本地端口: %d)", t.TargetHost, t.TargetPort, t.LocalPort),
		audit.RiskMedium, audit.ResultSuccess)

	c.JSON(http.StatusOK, gin.H{
		"tunnel_id":  t.ID,
		"local_addr": "127.0.0.1",
		"local_port": t.LocalPort,
		"target":     t.TargetHost,
		"note":       "connect to 127.0.0.1:<local_port> on the control server to reach the target",
	})
}

func (h *handlers) listTunnels(c *gin.Context) {
	if !h.tunnelCoord(c) {
		return
	}
	agentID := c.Param("id")
	if agentID != "" {
		a := h.reg.GetAgent(agentID)
		if a == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
			return
		}
		if !h.canSeeHost(c, a) {
			c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
	}
	var out []gin.H
	for _, t := range h.reg.TunnelCoordinator().List() {
		if agentID != "" && t.AgentID != agentID {
			continue
		}
		out = append(out, gin.H{
			"tunnel_id":   t.ID,
			"agent_id":    t.AgentID,
			"target_host": t.TargetHost,
			"target_port": t.TargetPort,
			"local_port":  t.LocalPort,
			"created_at":  t.Created,
		})
	}
	if out == nil {
		out = []gin.H{}
	}
	c.JSON(http.StatusOK, gin.H{"tunnels": out})
}

func (h *handlers) closeTunnel(c *gin.Context) {
	if !h.tunnelCoord(c) {
		return
	}
	agentID := c.Param("id")
	tid := c.Param("tid")

	t, ok := h.reg.TunnelCoordinator().Get(tid)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "tunnel not found"})
		return
	}
	if agentID != "" && t.AgentID != agentID {
		c.JSON(http.StatusForbidden, gin.H{"error": "tunnel does not belong to this host"})
		return
	}
	a := h.reg.GetAgent(t.AgentID)
	if a != nil && !h.canSeeHost(c, a) {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	if err := h.reg.TunnelCoordinator().Close(tid); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	h.recordAudit(c, "tunnel_close", "host", t.AgentID,
		fmt.Sprintf("关闭反向 TCP 隧道 %s (本地端口: %d, 目标: %s:%d)", t.ID, t.LocalPort, t.TargetHost, t.TargetPort),
		audit.RiskLow, audit.ResultSuccess)

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- Operator tunnel control plane (§3.9.1, watchmanctl helper API) --------

// watchmanctlReleaseBase points at the rolling latest-release download page
// for the watchmanctl CLI binaries.
const watchmanctlReleaseBase = "https://github.com/lizhixu/workbench/releases/latest/download/"

// registerOperatorTunnelRoutes mounts the operator tunnel control-plane API.
// Read endpoints (client-info, mine) are audit-exempt; closing a leg is
// audited. The data plane itself lives on the dedicated -opgrpc listener and
// never touches this port.
func (h *handlers) registerOperatorTunnelRoutes(authed *gin.RouterGroup) {
	g := authed.Group("/tunnels")
	g.GET("/client-info", h.tunnelClientInfo)
	g.GET("/mine", h.listMyOperatorTunnels)
	g.DELETE("/mine/:tid", h.closeMyOperatorTunnel)
}

// tunnelClientInfo tells the web console and the watchmanctl CLI whether the
// operator data plane is enabled, where to reach it, and where to download
// the CLI. Read-only and audit-exempt (the console probes it on page load).
func (h *handlers) tunnelClientInfo(c *gin.Context) {
	enabled := h.opGRPCAddr != "" && h.reg.TunnelCoordinator() != nil
	out := gin.H{
		"enabled": enabled,
		"cli": gin.H{
			"linux_amd64":   watchmanctlReleaseBase + "watchmanctl-linux-amd64",
			"linux_arm64":   watchmanctlReleaseBase + "watchmanctl-linux-arm64",
			"windows_amd64": watchmanctlReleaseBase + "watchmanctl-windows-amd64.exe",
			"darwin_amd64":  watchmanctlReleaseBase + "watchmanctl-darwin-amd64",
			"darwin_arm64":  watchmanctlReleaseBase + "watchmanctl-darwin-arm64",
		},
	}
	if enabled {
		out["endpoint"] = h.operatorEndpoint(c)
	}
	c.JSON(http.StatusOK, out)
}

// operatorEndpoint derives the host:port of the operator data plane: the port
// comes from the -opgrpc listen address, the host from the panel public URL
// setting, then the -public-url flag, else from the request's Host header.
func (h *handlers) operatorEndpoint(c *gin.Context) string {
	port := "9443"
	if _, p, err := net.SplitHostPort(h.opGRPCAddr); err == nil && p != "" && p != "0" {
		port = p
	}
	host := ""
	for _, candidate := range []string{h.settings.PanelSecurity().PublicURL, publicURL} {
		if candidate == "" {
			continue
		}
		if u, err := url.Parse(candidate); err == nil && u.Hostname() != "" {
			host = u.Hostname()
			break
		}
	}
	if host == "" {
		host = c.Request.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
	}
	// A bare bracketed IPv6 literal gets here when Host carried no port;
	// strip the brackets so JoinHostPort brackets it exactly once instead of
	// producing [[::1]]:9443.
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	}
	return net.JoinHostPort(host, port)
}

// listMyOperatorTunnels lists the caller's live operator tunnel legs.
func (h *handlers) listMyOperatorTunnels(c *gin.Context) {
	if !h.tunnelCoord(c) {
		return
	}
	legs := h.reg.TunnelCoordinator().ListOwnerLegs(auth.Username(c))
	if legs == nil {
		legs = []rpc.OwnerLeg{}
	}
	c.JSON(http.StatusOK, gin.H{"tunnels": legs})
}

// closeMyOperatorTunnel force-closes one of the caller's own tunnel legs
// (admins may close anyone's). Audited as op_tunnel_close.
func (h *handlers) closeMyOperatorTunnel(c *gin.Context) {
	if !h.tunnelCoord(c) {
		return
	}
	tid := c.Param("tid")
	isAdmin := auth.RoleOf(c) == auth.RoleAdmin
	err := h.reg.TunnelCoordinator().CloseOwnerLeg(tid, auth.Username(c), isAdmin)
	if err != nil {
		if strings.HasPrefix(err.Error(), "forbidden") {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	h.recordAudit(c, "op_tunnel_close", "tunnel", tid,
		fmt.Sprintf("手动关闭运维隧道腿 %s", tid), audit.RiskLow, audit.ResultSuccess)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
