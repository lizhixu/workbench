package cert

import (
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Handlers mounts the certificate hub REST API.
type Handlers struct {
	hub *Hub
	// issuing serializes Issue/Renew calls: ACME rate limits are strict and
	// two concurrent wildcard orders for the same zone would race.
	mu sync.Mutex
}

// NewHandlers creates the certificate hub handlers.
func NewHandlers(hub *Hub) *Handlers {
	return &Handlers{hub: hub}
}

// Register mounts routes. Config reads are open to all roles (the UI shows
// integration state); config writes, issue/renew and delete are gated to
// roles allowed to change system state by the caller (writeRG).
func (h *Handlers) Register(readRG, writeRG *gin.RouterGroup) {
	readRG.GET("/certs/config", h.getConfig)
	readRG.GET("/certs", h.listCerts)
	readRG.GET("/certs/:id", h.getCert)

	writeRG.PUT("/certs/config", h.putConfig)
	writeRG.POST("/certs/issue", h.issueCert)
	writeRG.POST("/certs/:id/renew", h.renewCert)
	writeRG.DELETE("/certs/:id", h.deleteCert)
}

func (h *Handlers) getConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.hub.GetConfig()})
}

func (h *Handlers) putConfig(c *gin.Context) {
	var cfg Config
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if cfg.Enabled {
		if strings.TrimSpace(cfg.BaseURL) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "启用时必须填写 dns-mng 服务地址"})
			return
		}
		if cfg.Username == "" || cfg.Password == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "启用时必须填写 dns-mng Basic Auth 账号密码"})
			return
		}
	}
	if err := h.hub.UpdateConfig(cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.hub.GetConfig()})
}

func (h *Handlers) listCerts(c *gin.Context) {
	list := h.hub.List()
	if list == nil {
		list = []*Certificate{}
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

func (h *Handlers) getCert(c *gin.Context) {
	crt, ok := h.hub.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "certificate not found"})
		return
	}
	crt.KeyPEM = "" // never expose the private key over REST
	c.JSON(http.StatusOK, gin.H{"data": crt})
}

func (h *Handlers) issueCert(c *gin.Context) {
	var req struct {
		Domains []string `json:"domains"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Domains) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "至少需要一个域名"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	// DNS-01 + propagation takes tens of seconds; run async and let the UI
	// poll the list.
	go func() {
		_, _ = h.hub.Issue(req.Domains)
	}()
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "message": "签发请求已受理，请稍后刷新列表"})
}

func (h *Handlers) renewCert(c *gin.Context) {
	id := c.Param("id")
	if _, ok := h.hub.Get(id); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "certificate not found"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	go func() {
		_, _ = h.hub.Renew(id)
	}()
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "message": "续期请求已受理，请稍后刷新列表"})
}

func (h *Handlers) deleteCert(c *gin.Context) {
	if err := h.hub.Delete(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
