package commands

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handlers exposes the saved-command library REST endpoints.
type Handlers struct {
	store *Store
}

// NewHandlers creates command library Handlers.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// Register mounts the command library routes on an authenticated group.
func (h *Handlers) Register(rg *gin.RouterGroup) {
	rg.GET("/commands", h.list)
	rg.POST("/commands", h.create)
	rg.PATCH("/commands/:id", h.update)
	rg.DELETE("/commands/:id", h.remove)
	rg.POST("/commands/:id/use", h.markUsed)
}

func (h *Handlers) list(c *gin.Context) {
	entries := h.store.List(currentUser(c), c.Query("shell"), c.Query("q"))
	c.JSON(http.StatusOK, gin.H{"data": entries})
}

func (h *Handlers) create(c *gin.Context) {
	var in Entry
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Only an admin may publish a command to everyone.
	if in.Shared && !isAdmin(c) {
		in.Shared = false
	}
	created, err := h.store.Create(in, currentUser(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": created})
}

func (h *Handlers) update(c *gin.Context) {
	var in Entry
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, err := h.store.Update(c.Param("id"), in, currentUser(c), isAdmin(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": updated})
}

func (h *Handlers) remove(c *gin.Context) {
	if err := h.store.Delete(c.Param("id"), currentUser(c), isAdmin(c)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) markUsed(c *gin.Context) {
	h.store.MarkUsed(c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func currentUser(c *gin.Context) string {
	return c.GetString("username")
}

func isAdmin(c *gin.Context) bool {
	return c.GetString("role") == "admin"
}
