package alert

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handlers bundles the alert REST endpoints.
type Handlers struct {
	store *Store
}

// NewHandlers creates alert Handlers.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// Register mounts alert routes on the given authenticated group.
func (h *Handlers) Register(rg *gin.RouterGroup) {
	rg.GET("/alerts/rules", h.listRules)
	rg.POST("/alerts/rules", h.createRule)
	rg.PUT("/alerts/rules/:id", h.updateRule)
	rg.DELETE("/alerts/rules/:id", h.deleteRule)
	rg.GET("/alerts/events", h.listEvents)
	rg.POST("/alerts/events/ack-all", h.ackAllEvents)
	rg.POST("/alerts/events/:id/ack", h.ackEvent)
	rg.GET("/alerts/webhook", h.getWebhook)
	rg.PUT("/alerts/webhook", h.setWebhook)
}

func (h *Handlers) listRules(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.ListRules()})
}

func (h *Handlers) createRule(c *gin.Context) {
	var r Rule
	if err := c.ShouldBindJSON(&r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	r.ID = "" // server-generated
	if err := h.store.CreateRule(&r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": r})
}

func (h *Handlers) updateRule(c *gin.Context) {
	var r Rule
	if err := c.ShouldBindJSON(&r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	r.ID = c.Param("id")
	if err := h.store.UpdateRule(&r); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": r})
}

func (h *Handlers) deleteRule(c *gin.Context) {
	if err := h.store.DeleteRule(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) listEvents(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.ListEvents(200)})
}

func (h *Handlers) ackEvent(c *gin.Context) {
	if err := h.store.AckEvent(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) ackAllEvents(c *gin.Context) {
	if err := h.store.AckAllEvents(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) getWebhook(c *gin.Context) {
	w := h.store.GetWebhook()
	// Don't expose the secret in full.
	if w.Secret != "" {
		w.Secret = "********"
	}
	c.JSON(http.StatusOK, gin.H{"data": w})
}

func (h *Handlers) setWebhook(c *gin.Context) {
	var w WebhookConfig
	if err := c.ShouldBindJSON(&w); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// If the secret is the mask, preserve the existing one.
	if w.Secret == "********" {
		existing := h.store.GetWebhook()
		w.Secret = existing.Secret
	}
	if err := h.store.SetWebhook(w); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}