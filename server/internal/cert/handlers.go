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
	readRG.GET("/certs/presets", h.listPresets)
	readRG.GET("/certs/accounts", h.listAccounts)
	readRG.GET("/certs", h.listCerts)
	readRG.GET("/certs/:id", h.getCert)

	writeRG.PUT("/certs/config", h.putConfig)
	writeRG.POST("/certs/accounts", h.createAccount)
	writeRG.PUT("/certs/accounts/:id", h.updateAccount)
	writeRG.DELETE("/certs/accounts/:id", h.deleteAccount)
	writeRG.POST("/certs/issue", h.issueCert)
	writeRG.POST("/certs/:id/renew", h.renewCert)
	writeRG.DELETE("/certs/:id", h.deleteCert)
}

func (h *Handlers) listPresets(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": Presets})
}

func (h *Handlers) listAccounts(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.hub.ListAccounts()})
}

type accountReq struct {
	Name         string `json:"name" binding:"required"`
	ProviderID   string `json:"provider_id"`
	DirectoryURL string `json:"directory_url" binding:"required"`
	Email        string `json:"email" binding:"required"`
	EABKeyID     string `json:"eab_key_id"`
	EABHMACKey   string `json:"eab_hmac_key"`
	IsDefault    bool   `json:"is_default"`
}

func (h *Handlers) createAccount(c *gin.Context) {
	var req accountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写完整账户信息"})
		return
	}
	acc := &ACMEAccount{
		Name:         strings.TrimSpace(req.Name),
		ProviderID:   req.ProviderID,
		DirectoryURL: strings.TrimSpace(req.DirectoryURL),
		Email:        strings.TrimSpace(req.Email),
		EABKeyID:     strings.TrimSpace(req.EABKeyID),
		EABHMACKey:   strings.TrimSpace(req.EABHMACKey),
		IsDefault:    req.IsDefault,
	}
	if err := h.hub.PutAccount(acc); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": acc.Redacted()})
}

func (h *Handlers) updateAccount(c *gin.Context) {
	id := c.Param("id")
	old, ok := h.hub.GetAccount(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
		return
	}
	var req accountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	old.Name = strings.TrimSpace(req.Name)
	old.ProviderID = req.ProviderID
	old.DirectoryURL = strings.TrimSpace(req.DirectoryURL)
	old.Email = strings.TrimSpace(req.Email)
	old.EABKeyID = strings.TrimSpace(req.EABKeyID)
	// Only overwrite HMAC key if a new one is supplied
	if req.EABHMACKey != "" && !strings.Contains(req.EABHMACKey, "••") {
		old.EABHMACKey = strings.TrimSpace(req.EABHMACKey)
	}
	old.IsDefault = req.IsDefault
	if err := h.hub.PutAccount(old); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": old.Redacted()})
}

func (h *Handlers) deleteAccount(c *gin.Context) {
	if err := h.hub.DeleteAccount(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
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
		Domains   []string `json:"domains"`
		AccountID string   `json:"account_id"`
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
		_, _ = h.hub.Issue(req.Domains, req.AccountID)
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
