package cert

import (
	"fmt"
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
// integration state); config writes, issue/import/renew and delete are gated to
// roles allowed to change system state by the caller (writeRG).
func (h *Handlers) Register(readRG, writeRG *gin.RouterGroup) {
	readRG.GET("/certs/config", h.getConfig)
	readRG.GET("/certs/presets", h.listPresets)
	readRG.GET("/certs/accounts", h.listAccounts)
	readRG.GET("/certs", h.listCerts)
	readRG.GET("/certs/:id", h.getCert)
	readRG.GET("/certs/manual/:id", h.getManualOrder)

	writeRG.PUT("/certs/config", h.putConfig)
	writeRG.POST("/certs/accounts", h.createAccount)
	writeRG.PUT("/certs/accounts/:id", h.updateAccount)
	writeRG.DELETE("/certs/accounts/:id", h.deleteAccount)
	writeRG.POST("/certs/issue", h.issueCert)
	writeRG.POST("/certs/manual/:id/confirm", h.confirmManualOrder)
	writeRG.DELETE("/certs/manual/:id", h.cancelManualOrder)
	writeRG.POST("/certs/import", h.importCert)
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
	Email        string `json:"email"` // optional; empty means the ACME newAccount request omits contact (RFC 8555 allows it)
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
		// Challenge pins the ACME challenge mechanism: "" (auto),
		// "http-01", "dns-01" or "dns-01-manual".
		Challenge string `json:"challenge"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Domains) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "至少需要一个域名或 IP"})
		return
	}
	ch := ChallengePreference(strings.TrimSpace(req.Challenge))
	if !ch.Valid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("未知的验证方式: %q", req.Challenge)})
		return
	}

	// Manual DNS-01 is a two-phase flow: create the order now and return the
	// TXT records for the administrator to provision; the UI then calls
	// confirmManualOrder after the records are in place.
	if ch == ChallengeDNS01Manual {
		h.mu.Lock()
		order, err := h.hub.StartManualDNSOrder(req.Domains, req.AccountID)
		h.mu.Unlock()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": order})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	// DNS-01 + propagation takes tens of seconds; run async and let the UI
	// poll the list.
	go func() {
		_, _ = h.hub.IssueWithChallenge(req.Domains, req.AccountID, ch)
	}()
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "message": "签发请求已受理，请稍后刷新列表"})
}

// getManualOrder returns the status and DNS records of a pending manual
// DNS-01 order.
func (h *Handlers) getManualOrder(c *gin.Context) {
	order, err := h.hub.GetPendingOrder(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": order})
}

// confirmManualOrder starts CA validation for a pending manual DNS-01 order.
// The administrator must have provisioned the TXT records first; validation
// runs async and the UI polls getManualOrder for progress.
func (h *Handlers) confirmManualOrder(c *gin.Context) {
	id := c.Param("id")
	order, err := h.hub.GetPendingOrder(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if order.Status != ManualOrderAwaitingDNS && order.Status != ManualOrderError {
		c.JSON(http.StatusBadRequest, gin.H{"error": "任务当前不可确认验证"})
		return
	}
	if order.Status == ManualOrderError && order.Terminal {
		c.JSON(http.StatusBadRequest, gin.H{"error": "CA 已对该订单作出终态判定（验证不通过），请重新发起签发"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	go func() {
		_, _ = h.hub.ConfirmManualDNSOrder(id)
	}()
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "message": "已通知 CA 开始验证，请稍后查询任务状态"})
}

// cancelManualOrder discards a pending manual DNS-01 order (awaiting_dns or
// error state). The CA-side order is simply left to expire; the administrator
// removes the provisioned TXT records at their DNS provider if desired.
func (h *Handlers) cancelManualOrder(c *gin.Context) {
	if err := h.hub.CancelManualDNSOrder(c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "已取消手动签发任务"})
}

func (h *Handlers) importCert(c *gin.Context) {
	var req struct {
		CertPEM string `json:"cert_pem"`
		KeyPEM  string `json:"key_pem"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	crt, err := h.hub.Import([]byte(req.CertPEM), []byte(req.KeyPEM))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	crt.KeyPEM = "" // never expose the private key over REST
	c.JSON(http.StatusOK, gin.H{"data": crt})
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
