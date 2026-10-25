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
	// panelCertInfo resolves (bound cert id, panel domain) from the panel
	// security settings for the used-by reverse lookup; injected at wiring
	// time to keep the cert package decoupled from the settings store.
	panelCertInfo func() (certID, domain string)
	// appDomains lists deployed applications with a bound domain, for the
	// same used-by lookup (one ref per domain binding, CertID included).
	appDomains func() []AppDomainRef
	// clearAppCertBindings drops the explicit CertID from every domain
	// binding that references a certificate, used by the delete-cert guard's
	// force path; injected at wiring time, nil keeps the guard read-only.
	clearAppCertBindings func(certID string) []string
}

// NewHandlers creates the certificate hub handlers. Both lookup deps may be
// nil (unit tests); the detail endpoint then simply omits those predicates.
func NewHandlers(hub *Hub, panelCertInfo func() (string, string), appDomains func() []AppDomainRef) *Handlers {
	return &Handlers{hub: hub, panelCertInfo: panelCertInfo, appDomains: appDomains}
}

// SetAppCertBindingClearer injects the force-delete hook that unreferences a
// certificate from every app domain binding (returns the affected app names).
func (h *Handlers) SetAppCertBindingClearer(fn func(certID string) []string) {
	h.clearAppCertBindings = fn
}

// Register mounts routes. Config reads are open to all roles (the UI shows
// integration state); config writes, issue/import/renew and delete are gated to
// roles allowed to change system state by the caller (writeRG).
func (h *Handlers) Register(readRG, writeRG *gin.RouterGroup) {
	readRG.GET("/certs/config", h.getConfig)
	readRG.GET("/certs/presets", h.listPresets)
	readRG.GET("/certs/accounts", h.listAccounts)
	readRG.GET("/certs/cover", h.getCertCover)
	readRG.GET("/certs", h.listCerts)
	readRG.GET("/certs/:id", h.getCert)
	readRG.GET("/certs/:id/detail", h.getCertDetail)
	readRG.GET("/certs/:id/download", h.downloadCert)
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
	writeRG.PUT("/certs/:id/remark", h.putCertRemark)
	writeRG.POST("/certs/:id/key", h.revealCertKey)
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
	list := h.hub.ListView()
	if list == nil {
		list = []*CertView{}
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

// getCertCover reports whether any stored certificate covers the given
// domain, so the app create page can pre-check coverage while the domain is
// being typed and the issuance flow can poll for completion. Matching goes
// through hub.FindByDomain — the exact rule the reverse-proxy binding
// applies — so the pre-check and the eventual bind can never disagree.
// Read-only and login-accessible like the detail endpoint (exempt from
// operation audit per §7, same example as detail/download); PEM material
// never crosses this boundary (json:"-" on Certificate).
func (h *Handlers) getCertCover(c *gin.Context) {
	domain := strings.TrimSpace(strings.ToLower(c.Query("domain")))
	if !validCoverDomain(domain) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "域名格式非法（须为主机名，通配符域名请先到证书中心签发）"})
		return
	}
	crt, ok := h.hub.FindByDomain(domain)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"covered": false}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"covered": true,
		"cert":    &CertView{Certificate: crt, Source: h.hub.sourceOf(crt), AccountName: h.hub.accountNameOf(crt)},
	}})
}

// validCoverDomain mirrors the reverse-proxy domain rule (apps' strict
// RFC-1035-ish hostname shape) so the create-page probe only green-lights
// domains the proxy binding itself would accept. Wildcards are rejected here
// as there: app-bound domains are plain hostnames, wildcard material must
// be issued in the cert center first and selected explicitly.
func validCoverDomain(d string) bool {
	if len(d) == 0 || len(d) > 253 {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
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

// getCertDetail serves the enriched X.509 view (fingerprint, serial, key
// algorithm, chain length, used-by reverse lookup) for the detail dialog.
// Read-only and login-accessible like getCert; the chain PEM in the response
// is public certificate material only — the private key leaves the hub via
// the explicit revealCertKey endpoint (adminOnly, audited), never here.
func (h *Handlers) getCertDetail(c *gin.Context) {
	var certID, domain string
	if h.panelCertInfo != nil {
		certID, domain = h.panelCertInfo()
	}
	var apps []AppDomainRef
	if h.appDomains != nil {
		apps = h.appDomains()
	}
	d, ok := h.hub.Detail(c.Param("id"), certID, domain, apps)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "certificate not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": d})
}

// downloadCert serves the stored certificate chain as a "<domain>.crt"
// attachment. The download stays public-chain-only — the private key is not
// bundled with it (servers need the key placed separately, and a download
// must not hand over the site's identity in one file); key viewing goes
// through revealCertKey instead. Read-only and login-accessible like the
// detail endpoint; the browser navigates to it with the JWT in the query
// string (the standard download-auth path shared with audit export /
// backup / file downloads).
func (h *Handlers) downloadCert(c *gin.Context) {
	crt, ok := h.hub.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "certificate not found"})
		return
	}
	if strings.TrimSpace(crt.CertPEM) == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "证书材料缺失，无法下载"})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadFilename(crt)))
	c.Data(http.StatusOK, "application/x-pem-file", []byte(crt.CertPEM))
}

// downloadFilename builds a filesystem-safe "<primary-domain>.crt" name for
// the chain download: "*.example.com" maps to "_wildcard.example.com.crt",
// remaining filename-unsafe characters collapse to "_", and a certificate
// without domains falls back to its id.
func downloadFilename(c *Certificate) string {
	base := c.ID
	if len(c.Domains) > 0 {
		base = strings.TrimSpace(c.Domains[0])
	}
	if base == "" {
		base = c.ID
	}
	base = strings.ReplaceAll(base, "*", "_wildcard")
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			return r
		}
		return '_'
	}, base)
	return base + ".crt"
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

// deleteCert removes a certificate.
//
// Two guards protect certificates that are still in use:
//
//   - Panel binding: when the panel security settings explicitly bind this
//     certificate (panelCertInfo), the delete is refused (409) and
//     ?force=true does NOT override it. The panel certificate can only be
//     unbound from the panel security settings; deleting it out from under
//     a running panel would make panelsec silently fall back to another
//     certificate (or fail outright once the hub is empty), so the operator
//     must unbind it there first.
//   - App domain bindings: when an app domain explicitly references the
//     certificate, the delete is refused (409) with the app names so the
//     operator rebinds those domains first; ?force=true clears the
//     references (the domains keep serving whatever cert is already on the
//     nginx host and fall back to deterministic best-match until rebound)
//     and deletes anyway.
func (h *Handlers) deleteCert(c *gin.Context) {
	id := c.Param("id")
	if h.panelCertInfo != nil {
		if panelCertID, _ := h.panelCertInfo(); panelCertID != "" && panelCertID == id {
			c.JSON(http.StatusConflict, gin.H{"error": "该证书正被面板证书绑定使用（系统设置 → 面板域名与证书）。请先解除面板证书绑定后再删除；force 参数不适用于面板绑定证书"})
			return
		}
	}
	if h.appDomains != nil {
		var names []string
		seen := make(map[string]bool)
		for _, ref := range h.appDomains() {
			if ref.CertID == id && !seen[ref.Name] {
				seen[ref.Name] = true
				names = append(names, ref.Name)
			}
		}
		if len(names) > 0 {
			force := c.Query("force") == "true" || c.Query("force") == "1"
			if !force {
				c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf(
					"证书已被以下应用绑定：%s。请先在应用详情-域名与反代中更换证书，或携带 force=true 强制删除（相关域名将回落为待重新绑定证书状态）",
					strings.Join(names, "、"))})
				return
			}
			if h.clearAppCertBindings != nil {
				_ = h.clearAppCertBindings(id)
			}
		}
	}
	if err := h.hub.Delete(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// putCertRemark edits the admin remark of one certificate. Metadata only;
// runs under the audited admin write group like every other cert mutation.
func (h *Handlers) putCertRemark(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Remark string `json:"remark"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, ok := h.hub.Get(id); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "certificate not found"})
		return
	}
	updated, err := h.hub.UpdateRemark(id, req.Remark)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated.CertPEM, updated.KeyPEM = "", "" // same material stripping as list/get
	c.JSON(http.StatusOK, gin.H{"data": updated})
}

// revealCertKey returns the stored private key in clear text for the detail
// dialog. Every routine response (list/get/detail/download) strips the key;
// this endpoint is the single explicit door, kept on the audited admin write
// group and recorded as cert_key_reveal / RiskHigh — the same convention as
// the AI API key reveal. POST (not GET) because the shared audit middleware
// skips GET requests.
func (h *Handlers) revealCertKey(c *gin.Context) {
	crt, ok := h.hub.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "certificate not found"})
		return
	}
	if strings.TrimSpace(crt.KeyPEM) == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "私钥材料缺失，无法查看"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"key_pem": crt.KeyPEM}})
}
