// Reverse TCP tunnel REST API (dynamic networking, §3.9).
//
// A tunnel is a port-forward through an agent: the control server listens
// on a loopback port and forwards connections to a target on the agent's
// network. Operators open tunnels to reach intranet services (databases,
// web consoles, SSH) on managed hosts without opening any inbound ports
// on those hosts.
package api

import (
	"net/http"

	"watchman/server/internal/auth"

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
	var body struct {
		TargetHost string `json:"target_host"`
		TargetPort uint32 `json:"target_port"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	t, err := h.reg.TunnelCoordinator().Open(c.Param("id"), body.TargetHost, body.TargetPort)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
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
	if err := h.reg.TunnelCoordinator().Close(c.Param("tid")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
