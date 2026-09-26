package settings

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handlers exposes the unified settings endpoints.
type Handlers struct {
	store *Store
}

// NewHandlers creates settings Handlers.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// RegisterUser mounts the per-user endpoints: GET on rg, PUT on rgWrite (the
// caller passes an audited group for writes). The routes are scoped to the
// calling user (/me), so no role gate is needed: a user can only ever read or
// write their own settings.
func (h *Handlers) RegisterUser(rg, rgWrite *gin.RouterGroup) {
	rg.GET("/me/settings", h.getUser)
	rgWrite.PUT("/me/settings", h.putUser)
}

// RegisterSystem mounts the control-plane-wide endpoints. Both groups must be
// admin-gated by the caller; writes additionally go through the audit group.
func (h *Handlers) RegisterSystem(rg, rgWrite *gin.RouterGroup) {
	rg.GET("/settings", h.getSystem)
	rgWrite.PUT("/settings", h.putSystem)
}

// SchemaEntry describes one setting key for the client, so pickers and forms
// are rendered from the server instead of hardcoding options twice.
type SchemaEntry struct {
	Key     string   `json:"key"`
	Scope   string   `json:"scope"`
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	Default any      `json:"default"`
	Enum    []string `json:"enum,omitempty"`
}

// Schema returns the registry entries of one scope.
func (s *Store) Schema(scope Scope) []SchemaEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []SchemaEntry{}
	for _, d := range Definitions {
		if d.Scope != scope {
			continue
		}
		out = append(out, SchemaEntry{
			Key:     d.Key,
			Scope:   string(d.Scope),
			Kind:    string(d.Kind),
			Title:   d.Title,
			Default: d.Default,
			Enum:    d.Enum,
		})
	}
	return out
}

type settingsResponse struct {
	Data   map[string]any `json:"data"`
	Schema []SchemaEntry  `json:"schema"`
	// StoredKeys lists the keys the user (or admin) explicitly saved.
	// Everything else in Data is a server-side default. The client needs the
	// distinction: e.g. the theme migration must not mistake the default
	// "dark" for an explicit user choice.
	StoredKeys []string `json:"stored_keys"`
}

type putBody struct {
	Data map[string]json.RawMessage `json:"data"`
}

func (h *Handlers) getUser(c *gin.Context) {
	username := c.GetString("username")
	c.JSON(http.StatusOK, settingsResponse{
		Data:       h.store.Effective(ScopeUser, username),
		Schema:     h.store.Schema(ScopeUser),
		StoredKeys: h.store.StoredKeys(ScopeUser, username),
	})
}

func (h *Handlers) putUser(c *gin.Context) {
	var body putBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(body.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data 不能为空"})
		return
	}
	if err := h.store.SetMany(ScopeUser, c.GetString("username"), body.Data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.getUser(c)
}

func (h *Handlers) getSystem(c *gin.Context) {
	c.JSON(http.StatusOK, settingsResponse{
		Data:       h.store.Effective(ScopeSystem, ""),
		Schema:     h.store.Schema(ScopeSystem),
		StoredKeys: h.store.StoredKeys(ScopeSystem, ""),
	})
}

func (h *Handlers) putSystem(c *gin.Context) {
	var body putBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(body.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data 不能为空"})
		return
	}
	if err := h.store.SetMany(ScopeSystem, "", body.Data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.getSystem(c)
}
