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
	admin.POST("/vault/credentials/:id/secret", h.revealSecret)
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

// get returns a credential's metadata with the secret masked. Routine
// responses never carry the plaintext (AGENTS.md B.7.4); an admin can fetch it
// explicitly via the audited revealSecret endpoint below. The plaintext stays
// available in-process (Store.Get) for terminal login.
func (h *Handlers) get(c *gin.Context) {
	cred, ok := h.store.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	masked := *cred
	masked.Secret = ""
	c.JSON(http.StatusOK, gin.H{"data": masked, "has_secret": cred.Secret != ""})
}

// revealSecret returns the plaintext secret on demand (admin-only route).
// POST rather than GET because the shared audit middleware skips GET, and every
// reveal must leave a high-risk audit row (vault_secret_reveal).
func (h *Handlers) revealSecret(c *gin.Context) {
	cred, ok := h.store.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"id":     cred.ID,
		"name":   cred.Name,
		"secret": cred.Secret,
	}})
}

func (h *Handlers) update(c *gin.Context) {
	var cred Credential
	if err := c.ShouldBindJSON(&cred); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id := c.Param("id")
	// Distinguish a missing credential (404) from an invalid body (400).
	if _, ok := h.store.Get(id); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}
	if err := h.store.Update(id, &cred); err != nil {
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
