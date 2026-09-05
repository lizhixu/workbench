package groups

import (
	"net/http"

	"watchman/server/internal/rpc"

	"github.com/gin-gonic/gin"
)

// Handlers exposes the group and grant REST endpoints.
type Handlers struct {
	store *Store
	reg   *rpc.Registry
}

// NewHandlers creates group Handlers. reg is used to keep host records in sync
// when a group is renamed or deleted, and to report per-group host counts.
func NewHandlers(store *Store, reg *rpc.Registry) *Handlers {
	return &Handlers{store: store, reg: reg}
}

// Register mounts read routes on rg and admin-only mutations on adminRG.
func (h *Handlers) Register(rg *gin.RouterGroup, adminRG *gin.RouterGroup) {
	rg.GET("/groups", h.list)
	rg.GET("/groups/:id", h.get)
	rg.GET("/groups/:id/users", h.listGrants)
	rg.GET("/me/groups", h.myGroups)

	adminRG.POST("/groups", h.create)
	adminRG.PATCH("/groups/:id", h.update)
	adminRG.DELETE("/groups/:id", h.remove)
	adminRG.POST("/groups/:id/users", h.setGrant)
	adminRG.DELETE("/groups/:id/users/:username", h.revokeGrant)
}

// GroupDTO adds the live host count to a stored group.
type GroupDTO struct {
	Group
	HostCount int `json:"host_count"`
	UserCount int `json:"user_count"`
}

func (h *Handlers) toDTO(g Group) GroupDTO {
	dto := GroupDTO{Group: g}
	if h.reg != nil {
		dto.HostCount = h.reg.CountByGroup(g.Name)
	}
	dto.UserCount = len(h.store.ListGrants(g.ID, ""))
	return dto
}

func (h *Handlers) list(c *gin.Context) {
	all := h.store.List()
	allowed, restricted := h.store.AllowedGroupNames(c.GetString("username"), c.GetString("role") == "admin")
	out := make([]GroupDTO, 0, len(all))
	for _, g := range all {
		if restricted && !allowed[g.Name] {
			continue
		}
		out = append(out, h.toDTO(g))
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "total": len(out)})
}

func (h *Handlers) get(c *gin.Context) {
	g, ok := h.store.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "分组不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.toDTO(g)})
}

// myGroups reports the caller's own scope so the UI can hide group pickers for
// unrestricted users and pre-filter host lists for restricted ones.
func (h *Handlers) myGroups(c *gin.Context) {
	allowed, restricted := h.store.AllowedGroupNames(c.GetString("username"), c.GetString("role") == "admin")
	names := make([]string, 0, len(allowed))
	for n := range allowed {
		names = append(names, n)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"restricted": restricted,
		"groups":     names,
	}})
}

func (h *Handlers) create(c *gin.Context) {
	var in Group
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	created, err := h.store.Create(in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.toDTO(created)})
}

func (h *Handlers) update(c *gin.Context) {
	var in Group
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	oldName, updated, err := h.store.Update(c.Param("id"), in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	moved := 0
	if h.reg != nil && oldName != updated.Name {
		moved = h.reg.RenameGroup(oldName, updated.Name)
	}
	c.JSON(http.StatusOK, gin.H{"data": h.toDTO(updated), "hosts_migrated": moved})
}

func (h *Handlers) remove(c *gin.Context) {
	name, err := h.store.Delete(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cleared := 0
	if h.reg != nil {
		cleared = h.reg.RenameGroup(name, "")
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "hosts_cleared": cleared})
}

func (h *Handlers) listGrants(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.ListGrants(c.Param("id"), "")})
}

func (h *Handlers) setGrant(c *gin.Context) {
	var body struct {
		Username string    `json:"username"`
		Role     GrantRole `json:"role"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.SetGrant(c.Param("id"), body.Username, body.Role, c.GetString("username")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) revokeGrant(c *gin.Context) {
	if err := h.store.RevokeGrant(c.Param("id"), c.Param("username")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
