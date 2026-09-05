package prefs

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handlers exposes the per-user terminal preference endpoints.
type Handlers struct {
	store *Store
}

// NewHandlers creates terminal preference Handlers.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// Register mounts the preference routes on an authenticated group. The routes
// are scoped to the calling user (/me), so no role gate is needed: a user can
// only ever read or write their own terminal appearance.
func (h *Handlers) Register(rg *gin.RouterGroup) {
	rg.GET("/me/term-prefs", h.get)
	rg.PUT("/me/term-prefs", h.set)
}

func (h *Handlers) get(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"data":   h.store.Get(c.GetString("username")),
		"themes": Themes(),
	})
}

func (h *Handlers) set(c *gin.Context) {
	var in Prefs
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	saved, err := h.store.Set(c.GetString("username"), in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": saved})
}
