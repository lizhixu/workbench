package policy

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// Handlers exposes the command policy and audit REST endpoints.
type Handlers struct {
	store *Store
}

// NewHandlers creates policy Handlers.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// Register mounts policy routes. Reading the policy and its audit trail is open
// to any authenticated user; rewriting it can switch off the high-risk command
// blocklist, so that goes on the caller-supplied admin group.
func (h *Handlers) Register(rg *gin.RouterGroup, admin *gin.RouterGroup) {
	if admin == nil {
		admin = rg
	}
	rg.GET("/policy/command", h.getPolicy)
	admin.PUT("/policy/command", h.setPolicy)
	rg.GET("/policy/command/audit", h.listAudit)
}

func (h *Handlers) getPolicy(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.GetPolicy()})
}

func (h *Handlers) setPolicy(c *gin.Context) {
	var p Policy
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.SetPolicy(p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "data": h.store.GetPolicy()})
}

func (h *Handlers) listAudit(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	username := c.Query("username")
	hostID := c.Query("host_id")
	entries := h.store.ListAudit(limit, username, hostID)
	c.JSON(http.StatusOK, gin.H{"data": entries})
}