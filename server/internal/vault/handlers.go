package vault

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handlers bundles the credential vault REST endpoints.
type Handlers struct {
	store *Store
}

// NewHandlers creates vault Handlers.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// Register mounts vault routes on the given authenticated group.
// All vault operations are admin-only.
func (h *Handlers) Register(rg *gin.RouterGroup) {
	admin := rg.Group("", requireAdmin())
	admin.GET("/vault/credentials", h.list)
	admin.POST("/vault/credentials", h.create)
	admin.GET("/vault/credentials/:id", h.get)
	admin.PUT("/vault/credentials/:id", h.update)
	admin.DELETE("/vault/credentials/:id", h.delete)
}

func requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString("role") != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin only"})
			return
		}
		c.Next()
	}
}

func (h *Handlers) list(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.List()})
}

func (h *Handlers) create(c *gin.Context) {
	var cred Credential
	if err := c.ShouldBindJSON(&cred); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	created, err := h.store.Create(&cred)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": created})
}

func (h *Handlers) get(c *gin.Context) {
	cred, ok := h.store.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": cred})
}

func (h *Handlers) update(c *gin.Context) {
	var cred Credential
	if err := c.ShouldBindJSON(&cred); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.Update(c.Param("id"), &cred); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) delete(c *gin.Context) {
	if err := h.store.Delete(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}