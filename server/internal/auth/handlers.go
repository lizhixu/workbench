package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handlers bundles the user-management REST endpoints.
type Handlers struct {
	store *Store
}

// NewHandlers creates an auth Handlers backed by the given store.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// Register mounts user-management routes on the given router group.
// The group is expected to already require authentication (Middleware applied).
func (h *Handlers) Register(rg *gin.RouterGroup) {
	// User management is admin-only. Operators/viewers can still read their
	// own profile via /auth/me if needed, but the admin user list is gated.
	rg.GET("/users", RequireRole(RoleAdmin), h.listUsers)
	rg.POST("/users", RequireRole(RoleAdmin), h.createUser)
	rg.PUT("/users/:username/password", h.updatePassword)
	rg.PUT("/users/:username/role", RequireRole(RoleAdmin), h.updateRole)
	rg.POST("/users/:username/reset-password", RequireRole(RoleAdmin), h.resetPassword)
	rg.DELETE("/users/:username", RequireRole(RoleAdmin), h.deleteUser)
}

// Login handles POST /auth/login. This is registered as a PUBLIC route (before
// the auth middleware) by the router.
func (h *Handlers) Login(c *gin.Context) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tok, u, err := h.store.Authenticate(body.Username, body.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token":      tok,
		"user":       gin.H{"username": u.Username, "role": u.Role},
		"expires_in": 86400,
	})
}

func (h *Handlers) listUsers(c *gin.Context) {
	users := h.store.List()
	c.JSON(http.StatusOK, gin.H{"data": users, "total": len(users)})
}

func (h *Handlers) createUser(c *gin.Context) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     Role   `json:"role"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u, err := h.store.Create(body.Username, body.Password, body.Role)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": u})
}

func (h *Handlers) updatePassword(c *gin.Context) {
	target := c.Param("username")
	caller := Username(c)
	// Non-admins may only change their own password.
	if target != caller && RoleOf(c) != RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "can only change your own password"})
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Admins changing other users' passwords don't supply old_password.
	if target == caller {
		if err := h.store.UpdatePassword(target, body.OldPassword, body.NewPassword); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	} else {
		if err := h.store.ResetPassword(target, body.NewPassword); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) updateRole(c *gin.Context) {
	var body struct {
		Role Role `json:"role"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.UpdateRole(c.Param("username"), body.Role); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) resetPassword(c *gin.Context) {
	var body struct {
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.ResetPassword(c.Param("username"), body.NewPassword); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) deleteUser(c *gin.Context) {
	if err := h.store.Delete(c.Param("username")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}